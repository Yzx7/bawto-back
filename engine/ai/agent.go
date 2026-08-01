// Package ai implementa el nodo `agent` del motor usando MiniMax a través de su
// endpoint compatible con Anthropic (SDK de Anthropic para Go + base URL de MiniMax).
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

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
	Branch string `json:"branch"`
	Reply  string `json:"reply,omitempty"`
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
	Steps int
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
	return &Agent{
		client:   anthropic.NewClient(option.WithAPIKey(apiKey), option.WithBaseURL(baseURL)),
		provider: provider,
		model:    model,
		rates:    rates,
	}
}

// Run ejecuta el nodo agent: responde al usuario y, si hay `outputs`, decide una rama.
func (a *Agent) Run(ctx context.Context, instruction string, vars map[string]string, outputs []string) (reply, branch string, err error) {
	reply, branch, _, err = a.RunWithHistoryUsage(ctx, instruction, vars, outputs, nil, false)
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

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 1024,
		System:    []anthropic.TextBlockParam{{Text: sys}},
		Messages:  anthropicMessages(history, vars),
	}
	if len(outputs) > 0 {
		tool, toolErr := branchTool(outputs, silent)
		if toolErr != nil {
			return "", "", Usage{}, toolErr
		}
		params.Tools = []anthropic.ToolUnionParam{tool}
		params.ToolChoice = anthropic.ToolChoiceParamOfTool(branchToolName)
		params.ToolChoice.OfTool.DisableParallelToolUse = anthropic.Bool(true)
		params.Temperature = anthropic.Float(0.2)
	}

	resp, err := a.client.Messages.New(ctx, params)
	if err != nil {
		return "", "", Usage{}, err
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
		reply, branch, err = parseBranchTool(resp.Content, outputs, silent)
		if err != nil {
			if outputErr, ok := err.(*OutputError); ok {
				stopDetail := fmt.Sprintf("stop_reason=%s", resp.StopReason)
				if outputErr.Detail == "" {
					outputErr.Detail = stopDetail
				} else {
					outputErr.Detail += " " + stopDetail
				}
			}
			return reply, branch, usage, err
		}
		return reply, branch, usage, nil
	}

	var text strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	reply = strings.TrimSpace(text.String())
	if !silent && reply == "" {
		return "", "", usage, &OutputError{Code: "empty_reply"}
	}
	return reply, "", usage, nil
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
	var selected *anthropic.ToolUseBlock
	blockTypes := make([]string, 0, len(content))
	for _, block := range content {
		blockTypes = append(blockTypes, block.Type)
		if block.Type != "tool_use" {
			continue
		}
		if selected != nil {
			return "", "", &OutputError{Code: "multiple_tool_calls"}
		}
		toolUse := block.AsToolUse()
		selected = &toolUse
	}

	if selected == nil {
		return "", "", &OutputError{
			Code:   "missing_tool_call",
			Detail: "blocks=" + strings.Join(blockTypes, ","),
		}
	}
	if selected.Name != branchToolName {
		return "", "", &OutputError{Code: "unexpected_tool", Detail: selected.Name}
	}

	var input branchToolInput
	decoder := json.NewDecoder(bytes.NewReader(selected.Input))
	decoder.DisallowUnknownFields()
	if decodeErr := decoder.Decode(&input); decodeErr != nil {
		return "", "", &OutputError{Code: "invalid_tool_input", Detail: decodeErr.Error()}
	}
	var extra any
	if trailingErr := decoder.Decode(&extra); trailingErr != io.EOF {
		return "", "", &OutputError{Code: "invalid_tool_input", Detail: "contenido JSON adicional"}
	}

	for _, output := range outputs {
		if input.Branch == output {
			branch = output
			break
		}
	}
	if branch == "" {
		return "", "", &OutputError{Code: "invalid_branch", Detail: input.Branch}
	}
	reply = strings.TrimSpace(input.Reply)
	if !silent && reply == "" {
		return "", "", &OutputError{Code: "empty_reply"}
	}
	if silent {
		reply = ""
	}
	return reply, branch, nil
}
