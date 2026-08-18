// Sonda efímera: comprueba si el proveedor configurado en COPILOT_AI_* entrega
// bloques de razonamiento y qué tool_choice acepta con thinking activo. Existe
// porque el comentario de copilot/provider.go solo prueba que DeepSeek RECHAZA
// el tool_choice forzado; no prueba que ENTREGUE thinking con tool_choice auto.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()
	apiKey := os.Getenv("COPILOT_AI_API_KEY")
	baseURL := os.Getenv("COPILOT_AI_BASE_URL")
	model := os.Getenv("COPILOT_AI_MODEL")
	provider := os.Getenv("COPILOT_AI_PROVIDER")
	if apiKey == "" || baseURL == "" || model == "" {
		fmt.Println("faltan COPILOT_AI_API_KEY / BASE_URL / MODEL en backend/.env")
		os.Exit(1)
	}
	fmt.Printf("proveedor=%s modelo=%s baseURL=%s\n\n", provider, model, baseURL)

	tools := []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
		Name:        "get_flow_outline",
		Description: anthropic.String("Devuelve el grafo del flujo actual."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties:  map[string]any{},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}}}

	auto := anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}
	any_ := anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{DisableParallelToolUse: anthropic.Bool(true)}}
	forced := anthropic.ToolChoiceUnionParam{OfTool: &anthropic.ToolChoiceToolParam{
		Name: "get_flow_outline", DisableParallelToolUse: anthropic.Bool(true)}}

	prompt := "Antes de nada, razona en voz alta sobre qué necesitas saber del flujo y luego llama a get_flow_outline."

	// El thinking se inyecta por WithJSONSet igual que en copilot/provider.go.
	thinkingOn := option.WithJSONSet("thinking", map[string]any{"type": "enabled", "budget_tokens": 2048})
	thinkingOff := option.WithJSONSet("thinking", map[string]string{"type": "disabled"})

	// Se prueban los dos: el que corre hoy y el V4-Pro recién anunciado, que
	// promete mejores capacidades agénticas. Un modelo que no exista devuelve
	// un error del proveedor y queda registrado como tal.
	models := []string{model}
	for _, candidate := range []string{"deepseek-v4-pro", "deepseek-v4"} {
		if candidate != model {
			models = append(models, candidate)
		}
	}

	run := func(label string, model string, choice anthropic.ToolChoiceUnionParam, opts ...option.RequestOption) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		client := anthropic.NewClient(append([]option.RequestOption{
			option.WithAPIKey(apiKey), option.WithBaseURL(baseURL)}, opts...)...)
		resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:      anthropic.Model(model),
			MaxTokens:  4096,
			Messages:   []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
			Tools:      tools,
			ToolChoice: choice,
		})
		if err != nil {
			fmt.Printf("[%s] ERROR: %s\n\n", label, oneLine(err.Error()))
			return
		}
		var kinds []string
		thinking, text := "", ""
		for _, block := range resp.Content {
			switch typed := block.AsAny().(type) {
			case anthropic.ThinkingBlock:
				kinds = append(kinds, "thinking")
				thinking += typed.Thinking
			case anthropic.TextBlock:
				kinds = append(kinds, "text")
				text += typed.Text
			case anthropic.ToolUseBlock:
				kinds = append(kinds, "tool_use:"+typed.Name)
			default:
				kinds = append(kinds, fmt.Sprintf("%T", typed))
			}
		}
		fmt.Printf("[%s] OK bloques=[%s]\n", label, strings.Join(kinds, " "))
		if thinking != "" {
			fmt.Printf("        thinking(%d chars): %s\n", len(thinking), oneLine(preview(thinking)))
		} else {
			fmt.Printf("        thinking: (vacío)\n")
		}
		if text != "" {
			fmt.Printf("        text(%d chars): %s\n", len(text), oneLine(preview(text)))
		}
		fmt.Println()
	}

	for _, m := range models {
		fmt.Printf("════════ modelo %s ════════\n", m)
		run("thinking ON  + tool_choice auto", m, auto, thinkingOn)
		run("thinking ON  + tool_choice any", m, any_, thinkingOn)
		run("thinking ON  + tool_choice tool(forzado)", m, forced, thinkingOn)
		run("thinking OFF + tool_choice any (lo que corre hoy)", m, any_, thinkingOff)
		run("sin campo thinking + tool_choice auto", m, auto)
	}

	// Streaming: lo que decide si el panel puede pintar token a token.
	fmt.Println("--- streaming, thinking ON + tool_choice auto ---")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := anthropic.NewClient(option.WithAPIKey(apiKey), option.WithBaseURL(baseURL), thinkingOn)
	stream := client.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
		Model:      anthropic.Model(model),
		MaxTokens:  4096,
		Messages:   []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
		Tools:      tools,
		ToolChoice: auto,
	})
	deltas := map[string]int{}
	first := map[string]time.Duration{}
	started := time.Now()
	for stream.Next() {
		event := stream.Current()
		if delta, ok := event.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
			kind := delta.Delta.Type
			deltas[kind]++
			if _, seen := first[kind]; !seen {
				first[kind] = time.Since(started)
			}
		}
	}
	if err := stream.Err(); err != nil {
		fmt.Printf("stream ERROR: %s\n", oneLine(err.Error()))
		return
	}
	if len(deltas) == 0 {
		fmt.Println("no llegó ni un delta (el endpoint no streamea de verdad)")
		return
	}
	for kind, count := range deltas {
		fmt.Printf("  %-22s %4d deltas · primero a los %s\n", kind, count, first[kind].Round(10*time.Millisecond))
	}
}

func preview(value string) string {
	runes := []rune(value)
	if len(runes) > 220 {
		return string(runes[:220]) + "…"
	}
	return value
}

func oneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
