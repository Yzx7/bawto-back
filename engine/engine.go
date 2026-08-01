package engine

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// State es el estado persistido de una conversación (dónde quedó + variables).
type State struct {
	// FlowID identifica el flujo dueño de esta conversación. El motor no lo lee
	// ni lo escribe —ejecuta el grafo que le pasen—; lo rellena el webhook al
	// persistir, porque con varios flujos `message` publicados hay que reanudar
	// en el mismo grafo en que se empezó y no en el que case con el turno actual.
	FlowID       string            `json:"flowId,omitempty"`
	NodeID       string            `json:"nodeId"`
	Vars         map[string]string `json:"vars"`
	History      []ChatMessage     `json:"history,omitempty"`
	WaitingSince time.Time         `json:"waitingSince,omitempty"`
	TimeoutHours int               `json:"timeoutHours,omitempty"`
	// ResumeDirect reintenta el nodo indicado por NodeID en vez de tratarlo como
	// un wait ya consumido. Se usa cuando falla una dependencia externa.
	ResumeDirect bool `json:"resumeDirect,omitempty"`
}

// ChatMessage es un turno visible de la conversación que se conserva mientras
// el flujo está activo. El adaptador IA lo convierte en mensajes reales
// user/assistant, de modo que un loop wait → agent recuerda lo conversado.
type ChatMessage struct {
	Role    string `json:"role"` // user | assistant
	Content string `json:"content"`
}

// Result es la salida de un avance del flujo.
type Result struct {
	Sends     []string       // mensajes a enviar al usuario (en orden)
	Templates []TemplateSend // plantillas proactivas (WhatsApp)
	State     *State         // estado a persistir si quedó en pausa (wait); nil si terminó
	Done      bool           // true si la conversación terminó
	Handoff   bool           // true si el flujo escaló a un humano (action: handoff)
}

type TemplateSend struct {
	Name, Language string
	Params         []string
}

// AgentRequest describe una ejecución del nodo `agent`. Es una estructura y no
// una lista de parámetros porque el nodo ha ido ganando dimensiones —historial,
// silencio, herramientas— y cada una obligaba a tocar todos los llamadores.
type AgentRequest struct {
	NodeID      string
	Instruction string
	Vars        map[string]string
	Outputs     []string
	// History llega solo con contextMode=recent; vacío en el resto.
	History []ChatMessage
	Silent  bool
	// Tools son las herramientas que el modelo puede llamar en este turno. Vacío
	// significa una sola petición al proveedor, sin bucle.
	Tools []NodeTool
}

// Deps inyecta las capacidades que dependen de servicios externos.
type Deps struct {
	// Context aporta datos del contacto/cobro asociado al mensaje entrante.
	Context map[string]string
	// InputType y MediaID describen la entrada actual (text/reply/image/other).
	InputType string
	MediaID   string
	WaID      string
	// Agent ejecuta un nodo IA: devuelve el texto a enviar y la rama de salida.
	// El motor no sabe si por debajo hubo una llamada o un bucle de herramientas;
	// eso lo decide el adaptador según lo que el nodo declare.
	Agent func(AgentRequest) (reply, branch string, err error)
	// Tool ejecuta una función/API externa y devuelve un resultado (string).
	Tool func(ref string, args, vars map[string]string) (result string, err error)
}

const maxSteps = 100

const (
	maxHistoryMessages = 30
	maxHistoryRunes    = 16000
	maxMessageRunes    = 4000
)

var varRe = regexp.MustCompile(`\{(\w+)\}`)

// Advance ejecuta el flujo desde el estado dado con la entrada del usuario.
// - state == nil: inicia desde el trigger (userInput = mensaje que disparó).
// - state != nil: reanuda desde el nodo `wait` guardado (userInput = respuesta).
func Advance(flow *Flow, state *State, userInput string, deps Deps) (Result, error) {
	vars := map[string]string{}
	var cur string

	if state != nil && state.TimeoutHours > 0 && !state.WaitingSince.IsZero() && time.Since(state.WaitingSince) > time.Duration(state.TimeoutHours)*time.Hour {
		state = nil
	}

	var history []ChatMessage
	keepHistory := flowUsesRecentContext(flow)
	if state == nil {
		vars["input"] = userInput
		cur = flow.next("trigger", "")
	} else {
		if keepHistory {
			history = append(history, state.History...)
		}
		for k, v := range state.Vars {
			vars[k] = v
		}
		vars["input"] = userInput
	}
	if keepHistory {
		history = appendChatMessage(history, "user", historyInput(userInput, deps.InputType))
	}

	if state != nil {
		if state.ResumeDirect {
			cur = state.NodeID
		} else if w := flow.node(state.NodeID); w != nil {
			if !inputMatches(w.Expect, deps.InputType) {
				prompt := "Necesito que respondas con un mensaje de texto."
				if w.Expect == "image" {
					prompt = "Necesito que envíes una imagen para continuar."
				}
				if keepHistory {
					history = appendChatMessage(history, "assistant", prompt)
				}
				return Result{Sends: []string{prompt}, State: &State{
					NodeID:       state.NodeID,
					Vars:         state.Vars,
					History:      history,
					WaitingSince: state.WaitingSince,
					TimeoutHours: state.TimeoutHours,
				}}, nil
			}
			if w.SaveAs != "" {
				value := userInput
				if deps.InputType == "image" && deps.MediaID != "" {
					value = deps.MediaID
					vars[w.SaveAs+"_media_id"] = deps.MediaID
				}
				vars[w.SaveAs] = value
				vars[w.SaveAs+"_type"] = deps.InputType
			}
			cur = flow.next(w.ID, "")
		} else {
			cur = flow.next("trigger", "")
		}
	}
	// El contexto externo gana sobre datos antiguos: el estado de facturación puede
	// haber cambiado entre mensajes mientras el flow estaba pausado en un wait.
	for k, v := range deps.Context {
		vars[k] = v
	}
	vars["input_type"] = deps.InputType
	if deps.MediaID != "" {
		vars["input_media_id"] = deps.MediaID
	}
	if deps.WaID != "" {
		vars["input_wa_id"] = deps.WaID
	}

	var sends []string
	var templates []TemplateSend
	var handoff bool
	for steps := 0; steps < maxSteps && cur != ""; steps++ {
		n := flow.node(cur)
		if n == nil {
			break
		}
		switch n.Kind {
		case "send":
			if n.TemplateName != "" {
				params := make([]string, len(n.TemplateParams))
				for i, p := range n.TemplateParams {
					params[i] = interpolate(p, vars)
				}
				language := n.TemplateLanguage
				if language == "" {
					language = "es"
				}
				templates = append(templates, TemplateSend{Name: n.TemplateName, Language: language, Params: params})
			} else {
				message := interpolate(n.Body, vars)
				sends = append(sends, message)
				if keepHistory {
					history = appendChatMessage(history, "assistant", message)
				}
			}
			cur = flow.next(cur, "")

		case "condition":
			cur = flow.next(cur, evalCondition(n.Expression, vars))

		case "action":
			switch n.Action {
			case "end":
				return Result{Sends: sends, Templates: templates, Done: true, Handoff: handoff}, nil
			case "set":
				for k, v := range n.Params {
					vars[k] = interpolate(v, vars)
				}
			case "handoff":
				// Escala a un humano: el caller silencia al bot en ese chat.
				handoff = true
			}
			cur = flow.next(cur, "") // tag: sin efecto por ahora

		case "wait":
			return Result{Sends: sends, Templates: templates, State: &State{
				NodeID:       n.ID,
				Vars:         vars,
				History:      history,
				WaitingSince: time.Now().UTC(),
				TimeoutHours: n.TimeoutHours,
			}, Handoff: handoff}, nil

		case "agent":
			if deps.Agent == nil {
				return Result{Sends: sends, Templates: templates, Handoff: handoff},
					fmt.Errorf("agente IA no configurado")
			}
			request := AgentRequest{
				NodeID:      n.ID,
				Instruction: interpolate(n.Instruction, vars),
				Vars:        agentContextVars(vars),
				Outputs:     n.Outputs,
				Silent:      n.Silent,
				Tools:       n.Tools,
			}
			// Solo los nodos con memoria reciben la conversación; se copia para que
			// el adaptador no pueda alterar el historial del motor.
			if n.ContextMode == "recent" {
				request.History = append([]ChatMessage(nil), history...)
			}
			reply, branch, err := deps.Agent(request)
			if err != nil {
				return Result{
					Sends: sends, Templates: templates, Handoff: handoff,
					State: &State{
						NodeID:       n.ID,
						Vars:         vars,
						History:      history,
						WaitingSince: time.Now().UTC(),
						TimeoutHours: 24,
						ResumeDirect: true,
					},
				}, err
			}
			// Un clasificador (silent) no habla: su rama ya lleva al mensaje oficial.
			if reply != "" && !n.Silent {
				sends = append(sends, reply)
				if keepHistory {
					history = appendChatMessage(history, "assistant", reply)
				}
			}
			cur = flow.next(cur, branch)

		case "tool":
			var result string
			var err error
			if deps.Tool == nil {
				err = fmt.Errorf("herramienta %q no configurada", n.ToolRef)
			} else {
				result, err = deps.Tool(n.ToolRef, n.Args, vars)
			}
			if n.SaveAs != "" {
				vars[n.SaveAs] = result
			}
			if err != nil {
				cur = flow.next(cur, "error")
			} else {
				cur = flow.next(cur, "ok")
			}

		default:
			cur = flow.next(cur, "")
		}
	}

	return Result{Sends: sends, Templates: templates, Done: true, Handoff: handoff}, nil
}

func historyInput(input, inputType string) string {
	if strings.TrimSpace(input) != "" {
		return input
	}
	switch inputType {
	case "image":
		return "[El usuario envió una imagen.]"
	case "audio":
		return "[El usuario envió un audio.]"
	case "video":
		return "[El usuario envió un video.]"
	case "document":
		return "[El usuario envió un documento.]"
	default:
		return ""
	}
}

func flowUsesRecentContext(flow *Flow) bool {
	for i := range flow.Nodes {
		if flow.Nodes[i].Kind == "agent" && flow.Nodes[i].ContextMode == "recent" {
			return true
		}
	}
	return false
}

func appendChatMessage(history []ChatMessage, role, content string) []ChatMessage {
	content = strings.TrimSpace(content)
	if content == "" || role != "user" && role != "assistant" {
		return history
	}
	runes := []rune(content)
	if len(runes) > maxMessageRunes {
		content = string(runes[:maxMessageRunes])
	}
	history = append(history, ChatMessage{Role: role, Content: content})
	for len(history) > maxHistoryMessages || chatHistoryRunes(history) > maxHistoryRunes {
		history = history[1:]
	}
	return history
}

func chatHistoryRunes(history []ChatMessage) int {
	total := 0
	for _, message := range history {
		total += len([]rune(message.Content))
	}
	return total
}

// agentContextVars limita el contexto implícito del proveedor al mensaje actual
// y su tipo. El resto de variables solo llega al agente cuando el autor del flujo
// la referencia explícitamente en la instrucción y el motor la interpola.
func agentContextVars(vars map[string]string) map[string]string {
	context := make(map[string]string, 2)
	for _, key := range []string{"input", "input_type"} {
		if value, ok := vars[key]; ok {
			context[key] = value
		}
	}
	return context
}

func inputMatches(expect, inputType string) bool {
	if expect == "" || expect == "any" {
		return true
	}
	if expect == "text" {
		return inputType == "text" || inputType == "reply"
	}
	return expect == inputType
}

// interpolate reemplaza {var} por su valor.
func interpolate(s string, vars map[string]string) string {
	return varRe.ReplaceAllStringFunc(s, func(m string) string {
		key := m[1 : len(m)-1]
		if v, ok := vars[key]; ok {
			return v
		}
		return m
	})
}

// evalCondition evalúa expresiones simples: `var == valor`, `var != valor`, o `var`
// (verdadero si no está vacío). Devuelve "true" o "false".
func evalCondition(expr string, vars map[string]string) string {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return "false"
	}
	op := ""
	if strings.Contains(expr, "==") {
		op = "=="
	} else if strings.Contains(expr, "!=") {
		op = "!="
	}
	if op == "" {
		v := vars[strings.TrimSpace(expr)]
		if v != "" && v != "false" && v != "0" {
			return "true"
		}
		return "false"
	}
	parts := strings.SplitN(expr, op, 2)
	left := vars[strings.TrimSpace(parts[0])]
	right := unquote(strings.TrimSpace(parts[1]))
	eq := left == right
	if op == "!=" {
		eq = !eq
	}
	if eq {
		return "true"
	}
	return "false"
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
