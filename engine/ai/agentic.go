package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/Yzx7/sacs-chatbots/engine"
)

// Bucle agéntico: el modelo puede llamar herramientas durante el turno antes de
// responder, en vez de decidir una rama en una sola llamada.
//
// Conviven los dos modos a propósito. Un nodo sin herramientas sigue por
// `RunWithHistoryUsage`, que es una sola petición y un coste plano; solo los
// nodos que declaran herramientas pagan el bucle.
//
// La diferencia de fondo con el bloque `tool` del grafo es quién decide: allí lo
// decide una arista y el resultado va a `saveAs`; aquí lo decide el modelo y el
// resultado vuelve a su contexto. Por eso no hay ramas `ok`/`error` — un fallo se
// le entrega en el propio `tool_result` marcado como error y él decide si
// reintenta, lo menciona o sigue.

// AgentTool es lo que se le ofrece al modelo. La configuración que fija el autor
// del flujo no viaja aquí: se aplica en el ejecutor, para que el modelo no pueda
// cambiar el alcance de la herramienta.
type AgentTool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// ToolExecutor ejecuta una llamada del modelo y devuelve el texto que verá como
// resultado. Un error no aborta el turno: se le entrega como resultado de error.
type ToolExecutor func(ctx context.Context, name string, input json.RawMessage) (string, error)

// maxAgentSteps acota las peticiones al modelo dentro de un turno. Sin tope, un
// modelo que se atasca llamando herramientas convierte un mensaje de WhatsApp en
// una factura abierta.
const maxAgentSteps = 6

// maxToolResultRunes recorta lo que se le devuelve al modelo. Un catálogo entero
// en un `tool_result` se come el contexto y encarece cada paso siguiente, porque
// el historial se reenvía completo en cada iteración.
const maxToolResultRunes = 4000

// RunAgenticUsage ejecuta un nodo `agent` con herramientas.
//
// El contrato de salida no cambia: el turno **termina** cuando el modelo llama a
// `select_flow_branch`, así que sigue habiendo exactamente una respuesta y una
// rama, y el grafo sigue decidiendo qué pasa después. `tool_choice: any` obliga a
// que cada paso sea una llamada a herramienta, de modo que el bucle no puede
// quedarse en texto libre sin elegir rama.
func (a *Agent) RunAgenticUsage(
	ctx context.Context,
	instruction string,
	vars map[string]string,
	outputs []string,
	history []engine.ChatMessage,
	silent bool,
	agentTools []AgentTool,
	exec ToolExecutor,
) (reply, branch string, usage Usage, err error) {
	if len(outputs) == 0 {
		return "", "", Usage{}, &OutputError{Code: "invalid_outputs", Detail: "un agente con herramientas necesita ramas"}
	}
	branchToolParam, toolErr := branchTool(outputs, silent)
	if toolErr != nil {
		return "", "", Usage{}, toolErr
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 2048,
		System:    []anthropic.TextBlockParam{{Text: agenticSystemPrompt(instruction, silent, agentTools)}},
		Messages:  anthropicMessages(history, vars),
		Tools:     append(toolParams(agentTools), branchToolParam),
		ToolChoice: anthropic.ToolChoiceUnionParam{
			OfAny: &anthropic.ToolChoiceAnyParam{DisableParallelToolUse: anthropic.Bool(true)},
		},
		Temperature: anthropic.Float(0.2),
	}

	usage = Usage{Provider: a.provider, Model: a.model, Rates: a.rates}

	for step := 0; step < maxAgentSteps; step++ {
		resp, callErr := a.client.Messages.New(ctx, params, a.tenantOptions()...)
		if callErr != nil {
			return "", "", usage, callErr
		}
		usage.accumulate(resp)
		usage.Steps = step + 1

		// La respuesta se devuelve **completa** al historial, no solo sus bloques
		// de texto y `tool_use`. M3 razona de forma entrelazada y exige conservar
		// esos bloques para continuar la cadena; `ToParam` los preserva todos.
		params.Messages = append(params.Messages, resp.ToParam())

		calls := toolUses(resp.Content)
		if len(calls) == 0 {
			return "", "", usage, &OutputError{
				Code:   "missing_tool_call",
				Detail: fmt.Sprintf("stop_reason=%s", resp.StopReason),
			}
		}

		// Elegir rama cierra el turno aunque venga acompañada de otras llamadas:
		// el modelo ya decidió y lo demás sobra.
		for _, call := range calls {
			if call.Name == branchToolName {
				reply, branch, err = parseBranchTool(resp.Content, outputs, silent)
				return reply, branch, usage, err
			}
		}

		// **Toda** llamada necesita su resultado, en el mismo mensaje y en orden.
		// Responder solo la primera deja la segunda huérfana y el proveedor rechaza
		// la petición siguiente con «tool call result does not follow tool call».
		// `disable_parallel_tool_use` pide una sola, pero no se puede depender de
		// que el proveedor lo respete: el protocolo es quien manda.
		results := make([]anthropic.ContentBlockParamUnion, 0, len(calls))
		for _, call := range calls {
			result, execErr := exec(ctx, call.Name, []byte(call.Input))
			isError := execErr != nil
			if isError {
				// El texto del error va al modelo, no al cliente: es información
				// para que decida, igual que cualquier otro resultado.
				result = "Error al ejecutar la herramienta: " + execErr.Error()
			}
			results = append(results,
				anthropic.NewToolResultBlock(call.ID, truncateRunes(result, maxToolResultRunes), isError))
		}
		params.Messages = append(params.Messages, anthropic.NewUserMessage(results...))
	}

	return "", "", usage, &OutputError{
		Code:   "tool_loop_exhausted",
		Detail: fmt.Sprintf("el agente no eligió rama en %d pasos", maxAgentSteps),
	}
}

func (u *Usage) accumulate(resp *anthropic.Message) {
	u.InputTokens += resp.Usage.InputTokens
	u.OutputTokens += resp.Usage.OutputTokens
	u.CacheReadInputTokens += resp.Usage.CacheReadInputTokens
	u.CacheCreationInputTokens += resp.Usage.CacheCreationInputTokens
	if resp.ID != "" {
		u.RequestID = resp.ID
	}
	if resp.Model != "" {
		u.Model = string(resp.Model)
	}
}

// toolUses devuelve **todas** las llamadas de la respuesta, en orden. El modelo
// puede emitir varias en un mismo mensaje y cada una exige su resultado.
func toolUses(blocks []anthropic.ContentBlockUnion) []anthropic.ToolUseBlock {
	var out []anthropic.ToolUseBlock
	for _, block := range blocks {
		if use, ok := block.AsAny().(anthropic.ToolUseBlock); ok {
			out = append(out, use)
		}
	}
	return out
}

func toolParams(agentTools []AgentTool) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(agentTools)+1)
	for _, item := range agentTools {
		schema := anthropic.ToolInputSchemaParam{}
		if properties, ok := item.InputSchema["properties"]; ok {
			schema.Properties = properties
		}
		if required, ok := item.InputSchema["required"].([]string); ok {
			schema.Required = required
		}
		schema.ExtraFields = map[string]any{"additionalProperties": false}
		out = append(out, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        item.Name,
			Description: anthropic.String(item.Description),
			InputSchema: schema,
		}})
	}
	return out
}

func agenticSystemPrompt(instruction string, silent bool, agentTools []AgentTool) string {
	var b strings.Builder
	b.WriteString("Eres el asistente de un chatbot de atención al cliente por WhatsApp. ")
	b.WriteString("Sigue la instrucción del flujo y responde al usuario en español natural para Perú, breve y claro. ")
	b.WriteString("No uses voseo, no mezcles idiomas y no agregues preguntas que la instrucción no solicite.\n")
	b.WriteString("Instrucción: " + instruction)
	if len(agentTools) > 0 {
		b.WriteString("\n\nTienes herramientas para consultar información real. Úsalas antes de afirmar " +
			"datos concretos en vez de suponerlos; si una herramienta no devuelve lo que buscabas, dilo con naturalidad.")
	}
	if silent {
		b.WriteString("\nNo redactes una respuesta para el cliente; solo selecciona la rama.")
	} else {
		b.WriteString("\nRedacta una respuesta natural para el cliente y selecciona la rama en la misma llamada estructurada.")
	}
	b.WriteString("\nCuando tengas lo necesario, termina llamando a `select_flow_branch`. Esa llamada cierra el turno.")
	return b.String()
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "\n…(resultado recortado)"
}
