package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Rates replica la forma del tarifario de engine/ai sin importar ese paquete:
// el Copilot es deliberadamente independiente del runtime que atiende WhatsApp.
// Precios en USD por un millón de tokens.
type Rates struct {
	InputPerMillion      float64
	OutputPerMillion     float64
	CacheReadPerMillion  float64
	CacheWritePerMillion float64
}

// copilotMaxTokens acota la salida de cada paso. Es generoso para que quepan los
// bloques de reasoning de los proveedores que razonan, además del tool_use.
const copilotMaxTokens = 8192

// AnthropicProvider implementa ModelProvider contra un endpoint compatible con
// la API de Anthropic (Anthropic, MiniMax, DeepSeek...). El estado de la
// conversación viaja en Continuation para preservar los bloques de reasoning
// entre pasos sin exponerlos ni persistirlos nunca.
type AnthropicProvider struct {
	client   anthropic.Client
	provider string
	model    string
	rates    Rates
	tenant   string
}

func NewAnthropicProvider(apiKey, baseURL, provider, model, reasoningEffort string, rates Rates) *AnthropicProvider {
	opts := []option.RequestOption{option.WithAPIKey(apiKey), option.WithBaseURL(baseURL)}
	opts = append(opts, reasoningRequestOptions(provider, reasoningEffort)...)
	return &AnthropicProvider{
		client:   anthropic.NewClient(opts...),
		provider: provider,
		model:    model,
		rates:    rates,
	}
}

// ForTenant aísla el caché de prefijo del proveedor por organización, igual que
// el agente del runtime. Es una copia superficial: comparte el cliente HTTP.
func (p *AnthropicProvider) ForTenant(tenantID string) *AnthropicProvider {
	copia := *p
	copia.tenant = strings.TrimSpace(tenantID)
	return &copia
}

func (p *AnthropicProvider) tenantOptions() []option.RequestOption {
	if p.tenant == "" {
		return nil
	}
	return []option.RequestOption{
		option.WithJSONSet("metadata", map[string]string{"user_id": p.tenant}),
	}
}

// conversation es el estado opaco que se devuelve en Continuation. Solo vive
// durante el turno y nunca se copia a TurnResult, logs ni trazas.
type conversation struct {
	messages []anthropic.MessageParam
}

func (p *AnthropicProvider) Next(ctx context.Context, req ModelRequest) (ModelResponse, error) {
	conv, _ := req.Continuation.(*conversation)
	if conv == nil {
		conv = &conversation{}
		conv.messages = append(conv.messages,
			anthropic.NewUserMessage(anthropic.NewTextBlock(initialPrompt(req.Initial))))
	}
	if len(req.ToolResults) > 0 {
		blocks := make([]anthropic.ContentBlockParamUnion, 0, len(req.ToolResults))
		for _, result := range req.ToolResults {
			if result.CallID == "" {
				// Sin tool_use previo que referenciar (un paso que devolvió solo
				// texto): se entrega como texto para no romper el protocolo
				// tool_use/tool_result.
				blocks = append(blocks, anthropic.NewTextBlock(string(result.Output)))
				continue
			}
			blocks = append(blocks, anthropic.NewToolResultBlock(result.CallID, string(result.Output), result.IsError))
		}
		conv.messages = append(conv.messages, anthropic.NewUserMessage(blocks...))
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: copilotMaxTokens,
		System:    []anthropic.TextBlockParam{{Text: req.SystemPrompt}},
		Messages:  conv.messages,
		Tools:     toolParamsFor(req.Tools),
	}
	requestOptions := p.tenantOptions()
	if req.RequireTerminal {
		// El último paso del presupuesto se reserva para el terminal: se fuerza
		// submit_proposal para que el turno no muera en una consulta más.
		params.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{
				Name:                   ToolSubmitProposal,
				DisableParallelToolUse: anthropic.Bool(true),
			},
		}
		// Forzar UNA tool concreta es lo único incompatible con el razonamiento:
		// DeepSeek responde 400 «Thinking mode does not support this tool_choice»
		// y Anthropic solo admite `auto` con extended thinking. Se apaga aquí y
		// solo aquí, porque este paso ya no diseña: únicamente redacta el
		// terminal con el candidato que los pasos anteriores dejaron listo.
		// Comprobado con cmd/copilotthinkprobe contra v4-flash y v4-pro.
		requestOptions = append(requestOptions, thinkingDisabledOption())
	} else {
		// `any` garantiza que cada paso produzca al menos una function call y que
		// el loop nunca reciba un mensaje sin tool_use que referenciar. `any` sí
		// convive con el razonamiento; el que no es `tool{nombre}`.
		params.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfAny: &anthropic.ToolChoiceAnyParam{DisableParallelToolUse: anthropic.Bool(true)},
		}
	}

	// Se streamea siempre, incluso sin sink instalado: es el mismo resultado y
	// evita que el panel dependa de un modo distinto del que se prueba.
	stream := p.client.Messages.NewStreaming(ctx, params, requestOptions...)
	var resp anthropic.Message
	toolArguments := map[int64]*strings.Builder{}
	// El input que venía en content_block_start, guardado antes de que
	// Accumulate lo mezcle con los deltas. Es el respaldo cuando la
	// concatenación de deltas no da JSON válido por sí sola.
	startInputs := map[int64]json.RawMessage{}
	for stream.Next() {
		event := stream.Current()
		switch typed := event.AsAny().(type) {
		case anthropic.ContentBlockStartEvent:
			if typed.ContentBlock.Type == "tool_use" && typed.ContentBlock.Input != nil {
				if raw, err := json.Marshal(typed.ContentBlock.Input); err == nil {
					startInputs[typed.Index] = raw
				}
			}
		case anthropic.ContentBlockDeltaEvent:
			if delta, ok := typed.Delta.AsAny().(anthropic.InputJSONDelta); ok {
				buffer := toolArguments[typed.Index]
				if buffer == nil {
					buffer = &strings.Builder{}
					toolArguments[typed.Index] = buffer
				}
				buffer.WriteString(delta.PartialJSON)
			}
		case anthropic.ContentBlockStopEvent:
			// Accumulate marshaliza el bloque justo al procesar este evento, así
			// que la reparación tiene que ir antes o el turno muere aquí.
			repairToolInput(&resp, typed.Index, toolArguments, startInputs)
		case anthropic.MessageStopEvent:
			// Un proveedor que no cierre algún bloque dejaría basura que revienta
			// al marshalizar el mensaje entero.
			for index := range resp.Content {
				repairToolInput(&resp, int64(index), toolArguments, startInputs)
			}
		}
		if err := resp.Accumulate(event); err != nil {
			return ModelResponse{}, err
		}
		p.relayDelta(ctx, req.Step, event)
	}
	if err := stream.Err(); err != nil {
		return ModelResponse{}, err
	}
	// La respuesta completa vuelve al historial: los bloques de reasoning del
	// proveedor se preservan para continuar la cadena. ToParam los conserva todos.
	conv.messages = append(conv.messages, resp.ToParam())

	calls := make([]FunctionCall, 0)
	var textBuilder strings.Builder
	var thoughtBuilder strings.Builder
	for _, block := range resp.Content {
		if use, ok := block.AsAny().(anthropic.ToolUseBlock); ok {
			calls = append(calls, FunctionCall{ID: use.ID, Name: use.Name, Arguments: json.RawMessage(use.Input)})
		} else if txt, ok := block.AsAny().(anthropic.TextBlock); ok {
			textBuilder.WriteString(txt.Text)
		} else if thk, ok := block.AsAny().(anthropic.ThinkingBlock); ok {
			thoughtBuilder.WriteString(thk.Thinking)
		}
	}

	usage := ModelUsage{
		Provider:                    p.provider,
		Model:                       string(resp.Model),
		ProviderRequestID:           resp.ID,
		InputTokens:                 resp.Usage.InputTokens,
		OutputTokens:                resp.Usage.OutputTokens,
		CacheReadInputTokens:        resp.Usage.CacheReadInputTokens,
		CacheWriteInputTokens:       resp.Usage.CacheCreationInputTokens,
		InputCostPerMillionUSD:      p.rates.InputPerMillion,
		OutputCostPerMillionUSD:     p.rates.OutputPerMillion,
		CacheReadCostPerMillionUSD:  p.rates.CacheReadPerMillion,
		CacheWriteCostPerMillionUSD: p.rates.CacheWritePerMillion,
	}
	if usage.Model == "" {
		usage.Model = p.model
	}
	usage.CostUSD = (float64(usage.InputTokens)*p.rates.InputPerMillion +
		float64(usage.OutputTokens)*p.rates.OutputPerMillion +
		float64(usage.CacheReadInputTokens)*p.rates.CacheReadPerMillion +
		float64(usage.CacheWriteInputTokens)*p.rates.CacheWritePerMillion) / 1_000_000

	return ModelResponse{Continuation: conv, Calls: calls, Text: textBuilder.String(), Thought: thoughtBuilder.String(), Usage: usage}, nil
}

// repairToolInput deja el `input` de un bloque tool_use como JSON válido antes
// de que el SDK lo marshalice.
//
// Existe por dos comportamientos reales de un endpoint compatible, ninguno de
// los cuales se notaba con Messages.New porque allí el cuerpo llegaba entero y
// ya parseado:
//
//  1. Una tool sin argumentos no trae `input` ni deltas, y el RawMessage vacío
//     falla al marshalizar con «unexpected end of JSON input».
//  2. Si el proveedor manda el input completo en content_block_start Y además
//     como input_json_delta, el SDK los concatena —solo reemplaza cuando el
//     valor previo es exactamente `{}`— y produce dos objetos pegados.
//
// Los deltas mandan sobre el valor inicial, que es la semántica de Anthropic.
func repairToolInput(message *anthropic.Message, index int64,
	buffers map[int64]*strings.Builder, startInputs map[int64]json.RawMessage) {
	if index < 0 || int(index) >= len(message.Content) {
		return
	}
	block := &message.Content[index]
	if block.Type != "tool_use" {
		return
	}
	// 1) Los deltas son la fuente correcta según el protocolo.
	if buffer, exists := buffers[index]; exists && buffer.Len() > 0 {
		if raw := buffer.String(); json.Valid([]byte(raw)) {
			block.Input = json.RawMessage(raw)
			return
		}
	}
	// 2) Si los deltas no forman JSON válido por sí solos, el input del start
	//    conserva los argumentos de verdad. Recuperarlos importa: caer al objeto
	//    vacío deja al modelo llamando a la tool sin lo que pidió.
	if raw, exists := startInputs[index]; exists && json.Valid(raw) {
		block.Input = raw
		return
	}
	// 3) Último recurso: un objeto vacío mantiene vivo el turno.
	if len(block.Input) == 0 || !json.Valid(block.Input) {
		block.Input = json.RawMessage("{}")
	}
}

// relayDelta traduce los eventos del stream del proveedor a StreamDelta. Solo
// salen razonamiento, texto y el nombre de una tool al abrirse su bloque: los
// input_json_delta se ignoran a propósito, porque llevan los argumentos de la
// function call y esos nunca cruzan al panel.
func (p *AnthropicProvider) relayDelta(ctx context.Context, step int, event anthropic.MessageStreamEventUnion) {
	switch typed := event.AsAny().(type) {
	case anthropic.ContentBlockStartEvent:
		if typed.ContentBlock.Type == "tool_use" && typed.ContentBlock.Name != "" {
			emitDelta(ctx, StreamDelta{Step: step, Kind: "tool", ToolName: typed.ContentBlock.Name})
		}
	case anthropic.ContentBlockDeltaEvent:
		switch delta := typed.Delta.AsAny().(type) {
		case anthropic.ThinkingDelta:
			if delta.Thinking != "" {
				emitDelta(ctx, StreamDelta{Step: step, Kind: "thinking", Content: delta.Thinking})
			}
		case anthropic.TextDelta:
			if delta.Text != "" {
				emitDelta(ctx, StreamDelta{Step: step, Kind: "text", Content: delta.Text})
			}
		}
	}
}

func toolParamsFor(defs []FunctionDefinition) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(defs))
	for _, def := range defs {
		schema := anthropic.ToolInputSchemaParam{}
		if properties, ok := def.InputSchema["properties"]; ok {
			schema.Properties = properties
		}
		if required, ok := def.InputSchema["required"].([]string); ok {
			schema.Required = required
		}
		schema.ExtraFields = map[string]any{"additionalProperties": false}
		out = append(out, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        def.Name,
			Description: anthropic.String(def.Description),
			InputSchema: schema,
		}})
	}
	return out
}

func initialPrompt(initial *InitialModelContext) string {
	if initial == nil {
		return "(sin contexto)"
	}
	var sb strings.Builder
	if len(initial.RecentConversation) > 0 {
		sb.WriteString("=== HISTORIAL DE CONVERSACIÓN DE ESTA SESIÓN ===\n")
		for _, msg := range initial.RecentConversation {
			sb.WriteString(fmt.Sprintf("[%s]: %s\n", strings.ToUpper(msg.Role), msg.Content))
		}
		sb.WriteString("=== FIN DEL HISTORIAL ===\n\n")
	}
	sb.WriteString("Petición del autor para este turno:\n")
	sb.WriteString(initial.UserRequest)
	raw, err := json.Marshal(initial)
	if err == nil {
		sb.WriteString("\n\nContexto del turno (JSON):\n")
		sb.WriteString(string(raw))
	}
	return sb.String()
}

// thinkingDisabledOption apaga el razonamiento en una petición concreta. Se usa
// solo en el paso terminal, que es el único que fuerza una tool por nombre.
func thinkingDisabledOption() option.RequestOption {
	return option.WithJSONSet("thinking", map[string]string{"type": "disabled"})
}

// reasoningRequestOptions configura el razonamiento por proveedor. Cada
// proveedor compatible entiende el campo a su manera, así que se inyecta por
// WithJSONSet (precedente: providerRequestOptions en engine/ai).
func reasoningRequestOptions(provider, effort string) []option.RequestOption {
	provider = strings.ToLower(strings.TrimSpace(provider))
	effortLevel := strings.ToLower(strings.TrimSpace(effort))
	switch provider {
	case "deepseek":
		// Aquí estuvo apagado el razonamiento del Copilot entero hasta el
		// 2026-08-17, por un 400 que solo ocurre al forzar UNA tool por nombre.
		// Ese caso es exclusivo del paso terminal y ahora se trata allí, así que
		// los pasos de diseño —donde está el valor de razonar— vuelven a pensar.
		// DeepSeek razona por defecto y no admite presupuesto, así que no se
		// inyecta nada: pedirle `budget_tokens` devuelve 400.
		return nil
	case "anthropic", "claude":
		if effortLevel == "" || effortLevel == "none" {
			return nil
		}
		budget := 4096
		switch effortLevel {
		case "low":
			budget = 2048
		case "high":
			budget = 6144
		}
		return []option.RequestOption{
			option.WithJSONSet("thinking", map[string]any{"type": "enabled", "budget_tokens": budget}),
		}
	default:
		// MiniMax-M3 y demás compatibles razonan por defecto y sus bloques se
		// preservan vía ToParam; no se inyecta nada.
		return nil
	}
}
