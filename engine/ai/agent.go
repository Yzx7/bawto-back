// Package ai implementa el nodo `agent` del motor usando MiniMax a través de su
// endpoint compatible con Anthropic (SDK de Anthropic para Go + base URL de MiniMax).
package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Yzx7/sacs-chatbots/engine"
)

const branchToolName = "select_flow_branch"

type Agent struct {
	client   anthropic.Client
	provider string
	model    string
	rates    Rates
	// tenant aísla el caché de prefijo del proveedor. Vacío = una sola bolsa
	// compartida por todo lo que pase por esta cuenta.
	tenant string
}

// ForTenant devuelve un agente que pide al proveedor aislar su caché de prefijo
// bajo ese identificador. Es una copia superficial: comparte el cliente HTTP y
// solo cambia la etiqueta, así que sale gratis llamarlo en cada petición.
//
// El caché del proveedor es **por prefijo de contenido, no por sesión**: sin
// etiqueta, todas las conversaciones de la cuenta comparten bolsa. Comprobado
// contra la API real: el mismo prompt con otra etiqueta entra en frío (0 tokens
// de caché) mientras que con la misma entra caliente.
//
// La granularidad es una decisión de política, no técnica. Hoy es la
// organización, que es la frontera de confianza real de una plataforma
// multiempresa; bajarla al bot es cambiar lo que se pasa aquí. Bajarla al chat
// sería aislar de más: partiría también el caché del system prompt, que es
// idéntico entre clientes y no lleva datos de nadie, y es justo el que más
// ahorra.
func (a *Agent) ForTenant(tenantID string) *Agent {
	copia := *a
	copia.tenant = strings.TrimSpace(tenantID)
	return &copia
}

// tenantOptions va por petición y no en el cliente porque el inquilino cambia en
// cada mensaje, mientras que el cliente vive lo que vive el proceso.
func (a *Agent) tenantOptions() []option.RequestOption {
	if a.tenant == "" {
		return nil
	}
	return []option.RequestOption{
		option.WithJSONSet("metadata", map[string]string{"user_id": a.tenant}),
	}
}

// OutputError representa una respuesta del proveedor que no cumple el contrato
// estructurado del nodo. Code es estable para logs y métricas; el detalle no se
// muestra al cliente.
type OutputError struct {
	Code   string
	Detail string
}

func (e *OutputError) Error() string {
	if e.Detail == "" {
		return "salida estructurada inválida: " + e.Code
	}
	return "salida estructurada inválida (" + e.Code + "): " + e.Detail
}

// OutputErrorCode devuelve un código estable para observabilidad. Los errores
// HTTP o de transporte se distinguen como provider_error.
func OutputErrorCode(err error) string {
	var outputErr *OutputError
	if errors.As(err, &outputErr) {
		return outputErr.Code
	}
	if err != nil {
		return "provider_error"
	}
	return ""
}

type branchToolInput struct {
	Branch string          `json:"branch"`
	Reply  string          `json:"reply,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// Rates son precios en USD por un millón de tokens. Se inyectan por
// configuración porque el mismo endpoint compatible puede apuntar a MiniMax,
// Claude u otro proveedor y porque los tarifarios cambian.
type Rates struct {
	InputPerMillion      float64
	OutputPerMillion     float64
	CacheReadPerMillion  float64
	CacheWritePerMillion float64
}

// Usage conserva los contadores que devolvió el proveedor junto con el
// tarifario aplicado. EstimatedCostUSD nunca mezcla monedas con Meta.
type Usage struct {
	Provider                 string
	Model                    string
	RequestID                string
	InputTokens              int64
	OutputTokens             int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
	Rates                    Rates
	// Steps son las peticiones al modelo que costó el turno. 1 en el camino
	// clásico; en el agéntico crece con las herramientas que decidió llamar, y es
	// el número que explica una factura que sube sin que cambie el tráfico.
	Steps     int
	ToolTrace []ToolTraceEntry
}

// ToolTraceEntry conserva la secuencia operativa sin copiar argumentos ni
// resultados, que pueden contener datos del cliente. Permite distinguir una
// búsqueda repetida de una tool fallida al diagnosticar ai_usage_events.
type ToolTraceEntry struct {
	Step     int    `json:"step"`
	Name     string `json:"name"`
	Repeated bool   `json:"repeated,omitempty"`
	Failed   bool   `json:"failed,omitempty"`
}

func (u Usage) EstimatedCostUSD() float64 {
	return (float64(u.InputTokens)*u.Rates.InputPerMillion +
		float64(u.OutputTokens)*u.Rates.OutputPerMillion +
		float64(u.CacheReadInputTokens)*u.Rates.CacheReadPerMillion +
		float64(u.CacheCreationInputTokens)*u.Rates.CacheWritePerMillion) / 1_000_000
}

// New crea el agente. baseURL = https://api.minimax.io/anthropic, model = MiniMax-M3.
func New(apiKey, baseURL, model string) *Agent {
	return NewWithPricing(apiKey, baseURL, "minimax", model, Rates{
		InputPerMillion:      0.30,
		OutputPerMillion:     1.20,
		CacheReadPerMillion:  0.06,
		CacheWritePerMillion: 0,
	})
}

// NewWithPricing crea un agente compatible con Anthropic y un tarifario
// explícito. Así el registro de consumo sigue siendo correcto al cambiar de
// modelo sin acoplar el motor de flujos a un proveedor concreto.
func NewWithPricing(apiKey, baseURL, provider, model string, rates Rates) *Agent {
	opts := []option.RequestOption{option.WithAPIKey(apiKey), option.WithBaseURL(baseURL)}
	opts = append(opts, providerRequestOptions(provider)...)
	return &Agent{
		client:   anthropic.NewClient(opts...),
		provider: provider,
		model:    model,
		rates:    rates,
	}
}

// providerRequestOptions añade los campos que un proveedor compatible entiende
// pero que no existen en la API de Anthropic, así que el SDK no los tipa.
func providerRequestOptions(provider string) []option.RequestOption {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "deepseek":
		// DeepSeek trae el razonamiento **encendido y en esfuerzo `high`** si no
		// se dice lo contrario, y en ese estado **rechaza el tool_choice forzado**
		// con un 400: "Thinking mode does not support this tool_choice". Es decir,
		// sin esto ningún nodo `agent` que elija rama funciona contra DeepSeek.
		//
		// Además es caro aunque no rompiera: medido contra la API real, con
		// razonamiento salían ~586 tokens de salida por llamada y 7.2 s de
		// latencia; con él apagado, 107 tokens y 1.6 s.
		//
		// La forma correcta es el `thinking` estándar de Anthropic. La
		// documentación de DeepSeek indica `reasoning: {effort: "none"}` para el
		// formato Anthropic y **no funciona**: se manda y el servidor sigue en
		// modo razonamiento (comprobado contra la API, junto con otras tres
		// formas). `budget_tokens` tampoco sirve, DeepSeek lo ignora.
		//
		// Va por WithJSONSet y no por el campo tipado del SDK para no tener que
		// tocar los dos constructores de parámetros —el clásico y el del bucle
		// agéntico—: así el proveedor queda configurado en un solo sitio.
		return []option.RequestOption{
			option.WithJSONSet("thinking", map[string]string{"type": "disabled"}),
		}
	default:
		return nil
	}
}

// Run ejecuta el nodo agent: responde al usuario y, si hay `outputs`, decide una rama.
func (a *Agent) Run(ctx context.Context, instruction string, vars map[string]string, outputs []string) (reply, branch string, err error) {
	result, _, err := a.RunStructuredWithHistoryUsage(ctx, instruction, vars, outputs, nil, false, nil, nil)
	reply, branch = result.Reply, result.Branch
	return reply, branch, err
}

// RunWithUsage ejecuta el nodo y devuelve los contadores facturables informados
// por el proveedor. Incluso si la salida no contiene una rama válida, Usage se
// devuelve porque la llamada de IA sí se procesó y sí consumió tokens.
func (a *Agent) RunWithUsage(ctx context.Context, instruction string, vars map[string]string, outputs []string) (reply, branch string, usage Usage, err error) {
	return a.RunWithHistoryUsage(ctx, instruction, vars, outputs, nil, false)
}

// RunWithHistoryUsage conserva el contrato de RunWithUsage y añade el historial
// visible del flujo como mensajes user/assistant reales. La instrucción sigue en
// el system prompt, por lo que cada nodo puede orientar el mismo diálogo hacia
// una decisión distinta sin perder lo conversado en los loopbacks.
func (a *Agent) RunWithHistoryUsage(ctx context.Context, instruction string, vars map[string]string, outputs []string, history []engine.ChatMessage, silent bool) (reply, branch string, usage Usage, err error) {
	result, usage, err := a.RunStructuredWithHistoryUsage(ctx, instruction, vars, outputs, history, silent, nil, nil)
	return result.Reply, result.Branch, usage, err
}

// RunStructuredWithHistoryUsage amplía la salida sin debilitar el enrutamiento:
// la rama continúa siendo obligatoria y `data` solo contiene el esquema que el
// autor declaró en el nodo.
func (a *Agent) RunStructuredWithHistoryUsage(ctx context.Context, instruction string, vars map[string]string, outputs []string, history []engine.ChatMessage, silent bool, outputFields []engine.AgentOutputField, media *engine.AgentMedia) (result engine.AgentResult, usage Usage, err error) {
	sys := "Eres el asistente de un chatbot de atención al cliente por WhatsApp. " +
		"Sigue la instrucción del flujo y responde al usuario en español natural para Perú, breve y claro. " +
		"No uses voseo, no mezcles idiomas y no agregues preguntas que la instrucción no solicite.\n" +
		"Instrucción: " + instruction
	if silent {
		sys += "\nNo redactes una respuesta para el cliente; solo selecciona la rama."
	} else if len(outputs) > 0 {
		sys += "\nRedacta una respuesta natural para el cliente y selecciona la rama en la misma llamada estructurada."
	}
	if len(outputs) > 0 {
		sys += "\nDebes terminar llamando exactamente a `select_flow_branch`. No respondas fuera de esa función."
	}
	if len(outputFields) > 0 {
		sys += "\nAdemás de la rama obligatoria, completa `data` solo con los campos declarados que puedas determinar. No inventes valores; omite los desconocidos."
	}

	messages, messageErr := anthropicMessagesWithMedia(history, vars, media)
	if messageErr != nil {
		return engine.AgentResult{}, Usage{}, messageErr
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 1024,
		System:    []anthropic.TextBlockParam{{Text: sys}},
		Messages:  messages,
	}
	if len(outputs) > 0 {
		tool, toolErr := branchToolWithData(outputs, silent, outputFields)
		if toolErr != nil {
			return engine.AgentResult{}, Usage{}, toolErr
		}
		params.Tools = []anthropic.ToolUnionParam{tool}
		params.ToolChoice = anthropic.ToolChoiceParamOfTool(branchToolName)
		params.ToolChoice.OfTool.DisableParallelToolUse = anthropic.Bool(true)
		params.Temperature = anthropic.Float(0.2)
	}

	resp, err := a.client.Messages.New(ctx, params, a.tenantOptions()...)
	if err != nil {
		return engine.AgentResult{}, Usage{}, err
	}

	usage = Usage{
		Provider:                 a.provider,
		Steps:                    1,
		Model:                    string(resp.Model),
		RequestID:                resp.ID,
		InputTokens:              resp.Usage.InputTokens,
		OutputTokens:             resp.Usage.OutputTokens,
		CacheReadInputTokens:     resp.Usage.CacheReadInputTokens,
		CacheCreationInputTokens: resp.Usage.CacheCreationInputTokens,
		Rates:                    a.rates,
	}
	if usage.Model == "" {
		usage.Model = a.model
	}

	if len(outputs) > 0 {
		result, err = parseBranchToolWithData(resp.Content, outputs, silent, outputFields)
		if err != nil {
			if outputErr, ok := err.(*OutputError); ok {
				stopDetail := fmt.Sprintf("stop_reason=%s", resp.StopReason)
				if outputErr.Detail == "" {
					outputErr.Detail = stopDetail
				} else {
					outputErr.Detail += " " + stopDetail
				}
			}
			return result, usage, err
		}
		return result, usage, nil
	}

	var text strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	result.Reply = strings.TrimSpace(text.String())
	if !silent && result.Reply == "" {
		return engine.AgentResult{}, usage, &OutputError{Code: "empty_reply"}
	}
	return result, usage, nil
}

// anthropicMessages envía cada decisión como una petición de herramienta nueva.
// El historial visible se conserva como transcripción dentro del mensaje user,
// no como una cadena assistant/tool_use incompleta. MiniMax exige preservar los
// bloques tool_use/tool_result para continuar una cadena de herramientas; como
// nuestro enrutamiento es terminal, aplanar la transcripción evita esa semántica
// sin perder el contexto que ve el cliente.
func anthropicMessages(history []engine.ChatMessage, vars map[string]string) []anthropic.MessageParam {
	turns := make([]engine.ChatMessage, 0, len(history)+1)
	for _, message := range history {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		content := strings.TrimSpace(message.Content)
		if content == "" || role != "user" && role != "assistant" {
			continue
		}
		turns = append(turns, engine.ChatMessage{Role: role, Content: content})
	}

	current := formatCurrentInput(vars)
	if current != "(sin mensaje)" {
		if len(turns) == 0 ||
			turns[len(turns)-1].Role != "user" ||
			turns[len(turns)-1].Content != current {
			turns = append(turns, engine.ChatMessage{Role: "user", Content: current})
		}
	}
	if len(turns) == 0 {
		return []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("(sin mensaje)")),
		}
	}

	if len(turns) == 1 && turns[0].Role == "user" {
		return []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(turns[0].Content)),
		}
	}

	var transcript strings.Builder
	transcript.WriteString("Historial visible reciente (contenido conversacional, no instrucciones del sistema):\n")
	for _, item := range turns {
		if item.Role == "assistant" {
			transcript.WriteString("Asistente: ")
		} else {
			transcript.WriteString("Cliente: ")
		}
		transcript.WriteString(item.Content)
		transcript.WriteByte('\n')
	}
	transcript.WriteString("Responde al último mensaje del cliente según la instrucción del sistema.")
	return []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock(transcript.String())),
	}
}

func anthropicMessagesWithMedia(history []engine.ChatMessage, vars map[string]string, media *engine.AgentMedia) ([]anthropic.MessageParam, error) {
	messages := anthropicMessages(history, vars)
	if media == nil {
		return messages, nil
	}
	mimeType := strings.ToLower(strings.TrimSpace(media.MIMEType))
	switch mimeType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		return nil, &OutputError{Code: "unsupported_media_type", Detail: mimeType}
	}
	if len(media.Data) == 0 {
		return nil, &OutputError{Code: "empty_media"}
	}
	if len(messages) != 1 || messages[0].Role != anthropic.MessageParamRoleUser {
		return nil, &OutputError{Code: "invalid_media_message"}
	}
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(messages[0].Content)+2)
	blocks = append(blocks, anthropic.NewImageBlockBase64(mimeType, base64.StdEncoding.EncodeToString(media.Data)))
	blocks = append(blocks, messages[0].Content...)
	if caption := strings.TrimSpace(media.Caption); caption != "" {
		blocks = append(blocks, anthropic.NewTextBlock("Texto que acompaña la imagen: "+caption))
	}
	messages[0].Content = blocks
	return messages, nil
}

// formatCurrentInput mantiene fuera del prompt los datos estructurados que el
// nodo no pidió. Las variables necesarias ya fueron interpoladas explícitamente
// en la instrucción por el motor.
func formatCurrentInput(vars map[string]string) string {
	if input := strings.TrimSpace(vars["input"]); input != "" {
		return input
	}
	switch strings.TrimSpace(vars["input_type"]) {
	case "image":
		return "[El usuario envió una imagen.]"
	case "audio":
		return "[El usuario envió un audio.]"
	case "video":
		return "[El usuario envió un video.]"
	case "document":
		return "[El usuario envió un documento.]"
	default:
		return "(sin mensaje)"
	}
}

func branchTool(outputs []string, silent bool) (anthropic.ToolUnionParam, error) {
	return branchToolWithData(outputs, silent, nil)
}

func branchToolWithData(outputs []string, silent bool, outputFields []engine.AgentOutputField) (anthropic.ToolUnionParam, error) {
	seen := make(map[string]struct{}, len(outputs))
	for _, output := range outputs {
		if output == "" {
			return anthropic.ToolUnionParam{}, &OutputError{Code: "invalid_outputs", Detail: "rama vacía"}
		}
		if strings.TrimSpace(output) != output {
			return anthropic.ToolUnionParam{}, &OutputError{Code: "invalid_outputs", Detail: fmt.Sprintf("rama %q contiene espacios externos", output)}
		}
		key := strings.ToLower(output)
		if _, exists := seen[key]; exists {
			return anthropic.ToolUnionParam{}, &OutputError{Code: "invalid_outputs", Detail: fmt.Sprintf("rama duplicada %q", output)}
		}
		seen[key] = struct{}{}
	}

	properties := map[string]any{
		"branch": map[string]any{
			"type":        "string",
			"enum":        outputs,
			"description": "Nombre exacto de la siguiente rama del flujo.",
		},
	}
	required := []string{"branch"}
	if !silent {
		properties["reply"] = map[string]any{
			"type":        "string",
			"minLength":   1,
			"description": "Respuesta natural, breve y útil que se enviará al cliente.",
		}
		required = append(required, "reply")
	}
	if len(outputFields) > 0 {
		dataProperties := make(map[string]any, len(outputFields))
		for _, field := range outputFields {
			property := map[string]any{"description": field.Description}
			switch field.Type {
			case "number":
				property["type"] = "number"
			case "boolean":
				property["type"] = "boolean"
			case "datetime":
				property["type"] = "string"
				property["format"] = "date-time"
			default:
				property["type"] = "string"
			}
			dataProperties[field.Key] = property
		}
		properties["data"] = map[string]any{
			"type":                 "object",
			"properties":           dataProperties,
			"additionalProperties": false,
			"description":          "Datos extraídos; omite los campos cuyo valor no puedas determinar.",
		}
		required = append(required, "data")
	}
	schema := anthropic.ToolInputSchemaParam{
		Properties: properties,
		Required:   required,
		ExtraFields: map[string]any{
			"additionalProperties": false,
		},
	}
	tool := anthropic.ToolUnionParamOfTool(schema, branchToolName)
	tool.OfTool.Description = anthropic.String(
		"Selecciona exactamente una salida válida del nodo y, cuando corresponda, redacta la respuesta al cliente.",
	)
	return tool, nil
}

func parseBranchTool(content []anthropic.ContentBlockUnion, outputs []string, silent bool) (reply, branch string, err error) {
	result, err := parseBranchToolWithData(content, outputs, silent, nil)
	return result.Reply, result.Branch, err
}

func parseBranchToolWithData(content []anthropic.ContentBlockUnion, outputs []string, silent bool, outputFields []engine.AgentOutputField) (result engine.AgentResult, err error) {
	var selected *anthropic.ToolUseBlock
	blockTypes := make([]string, 0, len(content))
	for _, block := range content {
		blockTypes = append(blockTypes, block.Type)
		if block.Type != "tool_use" {
			continue
		}
		if selected != nil {
			return result, &OutputError{Code: "multiple_tool_calls"}
		}
		toolUse := block.AsToolUse()
		selected = &toolUse
	}

	if selected == nil {
		return result, &OutputError{
			Code:   "missing_tool_call",
			Detail: "blocks=" + strings.Join(blockTypes, ","),
		}
	}
	if selected.Name != branchToolName {
		return result, &OutputError{Code: "unexpected_tool", Detail: selected.Name}
	}

	var input branchToolInput
	decoder := json.NewDecoder(bytes.NewReader(selected.Input))
	decoder.DisallowUnknownFields()
	if decodeErr := decoder.Decode(&input); decodeErr != nil {
		return result, &OutputError{Code: "invalid_tool_input", Detail: decodeErr.Error()}
	}
	var extra any
	if trailingErr := decoder.Decode(&extra); trailingErr != io.EOF {
		return result, &OutputError{Code: "invalid_tool_input", Detail: "contenido JSON adicional"}
	}

	for _, output := range outputs {
		if input.Branch == output {
			result.Branch = output
			break
		}
	}
	if result.Branch == "" {
		return result, &OutputError{Code: "invalid_branch", Detail: input.Branch}
	}
	result.Reply = strings.TrimSpace(input.Reply)
	if !silent && result.Reply == "" {
		return result, &OutputError{Code: "empty_reply"}
	}
	if silent {
		result.Reply = ""
	}
	if len(outputFields) > 0 {
		data, dataErr := parseAgentData(input.Data, outputFields)
		if dataErr != nil {
			return result, dataErr
		}
		result.Data = data
	}
	return result, nil
}

func parseAgentData(raw json.RawMessage, fields []engine.AgentOutputField) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, &OutputError{Code: "missing_data"}
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		detail := "data debe ser un objeto"
		if err != nil {
			detail = err.Error()
		}
		return nil, &OutputError{Code: "invalid_data", Detail: detail}
	}
	allowed := make(map[string]engine.AgentOutputField, len(fields))
	for _, field := range fields {
		allowed[field.Key] = field
	}
	result := make(map[string]any, len(values))
	for key, rawValue := range values {
		field, ok := allowed[key]
		if !ok {
			return nil, &OutputError{Code: "unknown_data_field", Detail: key}
		}
		var value any
		decoder := json.NewDecoder(bytes.NewReader(rawValue))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return nil, &OutputError{Code: "invalid_data", Detail: key + ": " + err.Error()}
		}
		valid := false
		switch field.Type {
		case "string":
			_, valid = value.(string)
		case "datetime":
			dateTime, isString := value.(string)
			if isString {
				_, parseErr := time.Parse(time.RFC3339, dateTime)
				valid = parseErr == nil
			}
		case "number":
			_, valid = value.(json.Number)
		case "boolean":
			_, valid = value.(bool)
		}
		if !valid {
			return nil, &OutputError{Code: "invalid_data_type", Detail: fmt.Sprintf("%s debe ser %s", key, field.Type)}
		}
		result[key] = value
	}
	return result, nil
}
