package engine

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// State es el estado persistido de una conversación (dónde quedó + variables).
type State struct {
	NodeID       string            `json:"nodeId"`
	Vars         map[string]string `json:"vars"`
	WaitingSince time.Time         `json:"waitingSince,omitempty"`
	TimeoutHours int               `json:"timeoutHours,omitempty"`
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

// Deps inyecta las capacidades que dependen de servicios externos.
type Deps struct {
	// Context aporta datos del contacto/cobro asociado al mensaje entrante.
	Context map[string]string
	// InputType y MediaID describen la entrada actual (text/reply/image/other).
	InputType string
	MediaID   string
	WaID      string
	// Agent ejecuta un nodo IA: devuelve el texto a enviar y la rama de salida.
	Agent func(instruction string, vars map[string]string, outputs []string) (reply, branch string, err error)
	// Tool ejecuta una función/API externa y devuelve un resultado (string).
	Tool func(ref string, args, vars map[string]string) (result string, err error)
}

const maxSteps = 100

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

	if state == nil {
		vars["input"] = userInput
		cur = flow.next("trigger", "")
	} else {
		for k, v := range state.Vars {
			vars[k] = v
		}
		vars["input"] = userInput
		if w := flow.node(state.NodeID); w != nil {
			if !inputMatches(w.Expect, deps.InputType) {
				prompt := "Necesito que respondas con un mensaje de texto."
				if w.Expect == "image" {
					prompt = "Necesito que envíes una imagen para continuar."
				}
				return Result{Sends: []string{prompt}, State: state}, nil
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
				sends = append(sends, interpolate(n.Body, vars))
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
			return Result{Sends: sends, Templates: templates, State: &State{NodeID: n.ID, Vars: vars, WaitingSince: time.Now().UTC(), TimeoutHours: n.TimeoutHours}, Handoff: handoff}, nil

		case "agent":
			var reply, branch string
			var err error
			if deps.Agent == nil {
				return Result{Sends: sends, Templates: templates, Handoff: handoff}, fmt.Errorf("agente IA no configurado")
			}
			reply, branch, err = deps.Agent(interpolate(n.Instruction, vars), vars, n.Outputs)
			if err != nil {
				return Result{Sends: sends, Templates: templates, Handoff: handoff}, err
			}
			if reply != "" {
				sends = append(sends, reply)
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
