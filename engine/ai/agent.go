// Package ai implementa el nodo `agent` del motor usando MiniMax a través de su
// endpoint compatible con Anthropic (SDK de Anthropic para Go + base URL de MiniMax).
package ai

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type Agent struct {
	client anthropic.Client
	model  string
}

// New crea el agente. baseURL = https://api.minimax.io/anthropic, model = MiniMax-M2.
func New(apiKey, baseURL, model string) *Agent {
	return &Agent{
		client: anthropic.NewClient(option.WithAPIKey(apiKey), option.WithBaseURL(baseURL)),
		model:  model,
	}
}

// Run ejecuta el nodo agent: responde al usuario y, si hay `outputs`, decide una rama.
func (a *Agent) Run(ctx context.Context, instruction string, vars map[string]string, outputs []string) (reply, branch string, err error) {
	sys := "Eres el asistente de un chatbot de atención al cliente por WhatsApp. " +
		"Sigue la instrucción del flujo y responde al usuario en español, breve y claro.\n" +
		"Instrucción: " + instruction
	if len(outputs) > 0 {
		sys += "\n\nAl terminar, escribe en la ÚLTIMA línea exactamente `DECISION: X`, " +
			"donde X es EXACTAMENTE una de estas opciones: " + strings.Join(outputs, ", ") + "."
	}

	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 1024,
		System:    []anthropic.TextBlockParam{{Text: sys}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(formatContext(vars))),
		},
	})
	if err != nil {
		return "", "", err
	}

	var text strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(t.Text)
		}
	}

	reply, branch = parseDecision(text.String(), outputs)
	if len(outputs) > 0 && branch == "" {
		return reply, "", fmt.Errorf("el agente no eligió una rama válida")
	}
	return reply, branch, nil
}

// formatContext arma el mensaje de usuario con el input + variables del flujo.
func formatContext(vars map[string]string) string {
	var b strings.Builder
	if input := vars["input"]; input != "" {
		b.WriteString("Mensaje del usuario: " + input + "\n")
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		if k != "input" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		b.WriteString("Datos:\n")
		for _, k := range keys {
			b.WriteString(fmt.Sprintf("- %s: %s\n", k, vars[k]))
		}
	}
	if b.Len() == 0 {
		return "(sin mensaje)"
	}
	return b.String()
}

// parseDecision separa la respuesta al usuario de la línea `DECISION: X`.
func parseDecision(text string, outputs []string) (reply, branch string) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	idx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		up := strings.ToUpper(strings.TrimSpace(lines[i]))
		if strings.HasPrefix(up, "DECISION:") {
			idx = i
			raw := strings.TrimSpace(lines[i][strings.Index(up, "DECISION:")+len("DECISION:"):])
			branch = matchOutput(raw, outputs)
			break
		}
	}
	if idx < 0 {
		return strings.TrimSpace(text), branch
	}
	kept := make([]string, 0, len(lines))
	for i, ln := range lines {
		if i != idx {
			kept = append(kept, ln)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n")), branch
}

// matchOutput busca la opción elegida (exacta o contenida).
func matchOutput(val string, outputs []string) string {
	for _, o := range outputs {
		if strings.EqualFold(strings.TrimSpace(o), val) {
			return o
		}
	}
	lv := strings.ToLower(val)
	for _, o := range outputs {
		if strings.Contains(lv, strings.ToLower(o)) {
			return o
		}
	}
	return ""
}
