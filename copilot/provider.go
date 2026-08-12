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
	if req.RequireTerminal {
		// El último paso del presupuesto se reserva para el terminal: se fuerza
		// submit_proposal para que el turno no muera en una consulta más.
		params.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{
				Name:                   ToolSubmitProposal,
				DisableParallelToolUse: anthropic.Bool(true),
			},
		}
	} else {
		// `any` garantiza que cada paso produzca al menos una function call y que
		// el loop nunca reciba un mensaje sin tool_use que referenciar.
		params.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfAny: &anthropic.ToolChoiceAnyParam{DisableParallelToolUse: anthropic.Bool(true)},
		}
	}

	resp, err := p.client.Messages.New(ctx, params, p.tenantOptions()...)
	if err != nil {
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

// reasoningRequestOptions configura el razonamiento por proveedor. Cada
// proveedor compatible entiende el campo a su manera, así que se inyecta por
// WithJSONSet (precedente: providerRequestOptions en engine/ai).
func reasoningRequestOptions(provider, effort string) []option.RequestOption {
	provider = strings.ToLower(strings.TrimSpace(provider))
	effortLevel := strings.ToLower(strings.TrimSpace(effort))
	switch provider {
	case "deepseek":
		// DeepSeek con razonamiento rechaza el tool_choice forzado con un 400, y
		// el loop de autoría necesita tool_choice. Se apaga el razonamiento aquí.
		return []option.RequestOption{
			option.WithJSONSet("thinking", map[string]string{"type": "disabled"}),
		}
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
