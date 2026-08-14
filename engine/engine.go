package engine

import (
	"encoding/json"
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
	AgentRole   string
	Instruction string
	Vars        map[string]string
	Outputs     []string
	// OutputFields es el enriquecimiento opcional. Outputs sigue siendo el
	// contrato obligatorio que decide la conexión del grafo.
	OutputFields []AgentOutputField
	Accepts      []string
	// Media solo puede ser hidratada por el adaptador externo después de validar
	// ownership. El grafo nunca suministra bytes, URLs ni IDs arbitrarios.
	Media *AgentMedia
	// History llega solo con contextMode=recent; vacío en el resto.
	History []ChatMessage
	Silent  bool
	// Tools son las herramientas que el modelo puede llamar en este turno. Vacío
	// significa una sola petición al proveedor, sin bucle.
	Tools []NodeTool
}

type AgentMedia struct {
	Data     []byte
	MIMEType string
	Caption  string
}

// AgentResult conserva la decisión obligatoria del grafo y los datos
// declarados que el agente logró extraer. Data nunca controla conexiones.
type AgentResult struct {
	Reply  string
	Branch string
	Data   map[string]any
}

// InboundInput conserva el sobre agnóstico que las tools y variables del flujo
// pueden consultar. Los campos planos de Deps siguen existiendo como aliases
// durante la transición de los flujos publicados.
type InboundInput struct {
	ID                  string
	EventType           string
	ContentType         string
	Text                string
	Caption             string
	MediaID             string
	MimeType            string
	MediaSHA256         string
	ReplyTo             string
	Forwarded           bool
	FrequentlyForwarded bool
	ReactionEmoji       string
	ReactionMessageID   string
	ReactionRemoved     bool
}

// Deps inyecta las capacidades que dependen de servicios externos.
type Deps struct {
	// Context aporta datos del contacto/cobro asociado al mensaje entrante.
	Context map[string]string
	// InputType y MediaID describen la entrada actual (text/reply/image/other).
	InputType string
	MediaID   string
	WaID      string
	Input     InboundInput
	// Agent ejecuta un nodo IA: devuelve el texto a enviar y la rama de salida.
	// El motor no sabe si por debajo hubo una llamada o un bucle de herramientas;
	// eso lo decide el adaptador según lo que el nodo declare.
	Agent func(AgentRequest) (reply, branch string, err error)
	// AgentStructured es el contrato nuevo. Se prefiere cuando está configurado;
	// Agent queda como adaptador de compatibilidad para integraciones existentes.
	AgentStructured func(AgentRequest) (AgentResult, error)
	// Tool ejecuta una función/API externa y devuelve un resultado (string).
	Tool func(ref string, args, vars map[string]string) (result string, err error)
}

const maxSteps = 100

const (
	maxHistoryMessages = 30
	maxHistoryRunes    = 16000
	maxMessageRunes    = 4000
)

var varRe = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_.]*)\}`)

// Advance ejecuta el flujo desde el estado dado con la entrada del usuario.
// - state == nil: inicia desde el trigger (userInput = mensaje que disparó).
// - state != nil: reanuda desde el nodo `wait` guardado (userInput = respuesta).
func Advance(flow *Flow, state *State, userInput string, deps Deps) (Result, error) {
	inputType := deps.Input.ContentType
	if inputType == "" {
		inputType = deps.InputType
	}
	mediaID := deps.Input.MediaID
	if mediaID == "" {
		mediaID = deps.MediaID
	}
	inputID := deps.Input.ID
	if inputID == "" {
		inputID = deps.WaID
	}
	vars := map[string]string{}
	var cur string

	if state != nil && state.TimeoutHours > 0 && !state.WaitingSince.IsZero() && time.Since(state.WaitingSince) > time.Duration(state.TimeoutHours)*time.Hour {
		state = nil
	}

	var history []ChatMessage
	keepHistory := flowUsesRecentContext(flow)
	if state == nil {
		vars["input"] = userInput
		cur = flow.start(inputType)
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
		history = appendChatMessage(history, "user", historyInput(userInput, inputType))
	}

	if state != nil {
		if state.ResumeDirect {
			cur = state.NodeID
		} else if w := flow.node(state.NodeID); w != nil {
			// Los waits publicados antiguos podían filtrar y ramificar por formato.
			// Se conservan al ejecutarlos; un wait nuevo no lleva expect/accepts y
			// consume cualquier evento entrante por su única salida.
			if legacyTypedWait(w) && !inputMatches(w.Expect, w.Accepts, inputType) {
				prompt := expectedInputPrompt(w.Expect, w.Accepts)
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
				saveInboundEnvelope(vars, w.SaveAs, userInput, inputType, inputID, mediaID, deps.Input)
			}
			if legacyTypedWait(w) && len(w.Accepts) > 1 {
				cur = flow.next(w.ID, normalizedInputType(inputType))
			} else {
				cur = flow.next(w.ID, "")
			}
		} else {
			cur = flow.start(inputType)
		}
	}
	// El contexto externo gana sobre datos antiguos: el estado de facturación puede
	// haber cambiado entre mensajes mientras el flow estaba pausado en un wait.
	for k, v := range deps.Context {
		vars[k] = v
	}
	vars["input_type"] = inputType
	vars["input.id"] = inputID
	vars["input.eventType"] = deps.Input.EventType
	vars["input.contentType"] = inputType
	vars["input.text"] = userInput
	vars["input.caption"] = deps.Input.Caption
	vars["input.media.id"] = mediaID
	vars["input.media.mimeType"] = deps.Input.MimeType
	vars["input.media.sha256"] = deps.Input.MediaSHA256
	vars["input.replyTo"] = deps.Input.ReplyTo
	vars["input.forwarded"] = fmt.Sprint(deps.Input.Forwarded)
	vars["input.frequentlyForwarded"] = fmt.Sprint(deps.Input.FrequentlyForwarded)
	vars["input.reaction.emoji"] = deps.Input.ReactionEmoji
	vars["input.reaction.targetMessageId"] = deps.Input.ReactionMessageID
	if deps.Input.ReactionRemoved {
		vars["input.reaction.removed"] = "true"
	} else {
		vars["input.reaction.removed"] = "false"
	}
	if mediaID != "" {
		vars["input_media_id"] = mediaID
	}
	if inputID != "" {
		vars["input_wa_id"] = inputID
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

		case "router":
			cur = flow.next(cur, evalRouter(n.Cases, vars))

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
			if deps.AgentStructured == nil && deps.Agent == nil {
				return Result{Sends: sends, Templates: templates, Handoff: handoff},
					fmt.Errorf("agente IA no configurado")
			}
			request := AgentRequest{
				NodeID:       n.ID,
				AgentRole:    n.AgentRole,
				Instruction:  interpolate(n.Instruction, vars),
				Vars:         agentContextVars(vars),
				Outputs:      n.Outputs,
				OutputFields: append([]AgentOutputField(nil), n.OutputFields...),
				Accepts:      append([]string(nil), n.Accepts...),
				Silent:       n.Silent,
				Tools:        n.Tools,
			}
			// Solo los nodos con memoria reciben la conversación; se copia para que
			// el adaptador no pueda alterar el historial del motor.
			if n.ContextMode == "recent" {
				request.History = append([]ChatMessage(nil), history...)
			}
			var agentResult AgentResult
			var err error
			if deps.AgentStructured != nil {
				agentResult, err = deps.AgentStructured(request)
			} else {
				agentResult.Reply, agentResult.Branch, err = deps.Agent(request)
			}
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
			if n.SaveAs != "" {
				saveAgentResult(vars, n.SaveAs, agentResult, n.OutputFields)
			}
			// La respuesta puede limitarse a ramas concretas. El caso principal es
			// un orquestador que pregunta en `aclarar`, pero calla cuando ya entrega
			// el turno a un especialista que responderá en este mismo avance.
			if agentResult.Reply != "" && agentShouldReply(n, agentResult.Branch) {
				sends = append(sends, agentResult.Reply)
				if keepHistory {
					history = appendChatMessage(history, "assistant", agentResult.Reply)
				}
			}
			cur = flow.next(cur, agentResult.Branch)

		case "tool":
			var result string
			var err error
			if deps.Tool == nil {
				err = fmt.Errorf("herramienta %q no configurada", n.ToolRef)
			} else {
				result, err = deps.Tool(n.ToolRef, interpolateArgs(n.Args, vars), vars)
			}
			if n.SaveAs != "" {
				saveToolResult(vars, n.SaveAs, result)
			}
			if err != nil {
				// La causa se expone como `<saveAs>.error` para que la rama de
				// error pueda decírsela al cliente. Sin esto, el motor descartaba
				// el mensaje de dominio que la herramienta se había molestado en
				// conservar —«stock insuficiente para X (disponible: 2)»— y el
				// flujo solo podía responder un genérico.
				if n.SaveAs != "" {
					vars[n.SaveAs+".error"] = toolErrorText(err)
				}
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

func agentShouldReply(node *Node, branch string) bool {
	if node.Silent {
		return false
	}
	if len(node.ReplyOn) == 0 {
		return true
	}
	for _, allowed := range node.ReplyOn {
		if allowed == branch {
			return true
		}
	}
	return false
}

func interpolateArgs(args map[string]string, vars map[string]string) map[string]string {
	if len(args) == 0 {
		return nil
	}
	result := make(map[string]string, len(args))
	for key, value := range args {
		trimmed := strings.TrimSpace(value)
		if match := varRe.FindStringSubmatch(trimmed); len(match) == 2 && match[0] == trimmed {
			if _, exists := vars[match[1]]; !exists {
				// Un outputField desconocido se omite por contrato. Si el argumento
				// opcional conservara el marcador literal, terminaría enviando
				// "{receipt.amount}" a la API en vez de no enviar el campo.
				continue
			}
		}
		result[key] = interpolate(value, vars)
	}
	return result
}

func legacyTypedWait(wait *Node) bool {
	return wait != nil && (len(wait.Accepts) > 0 || wait.Expect != "" && wait.Expect != "any")
}

func saveInboundEnvelope(vars map[string]string, prefix, text, inputType, inputID, mediaID string, input InboundInput) {
	// Valor plano y aliases con guion bajo: compatibilidad con flujos ya creados.
	value := text
	if mediaID != "" {
		value = mediaID
	}
	vars[prefix] = value
	vars[prefix+"_type"] = inputType
	vars[prefix+"_media_id"] = mediaID

	// Sobre estructurado para routers, tools y el futuro builder visual.
	vars[prefix+".id"] = inputID
	vars[prefix+".eventType"] = input.EventType
	vars[prefix+".contentType"] = inputType
	vars[prefix+".text"] = text
	vars[prefix+".caption"] = input.Caption
	vars[prefix+".media.id"] = mediaID
	vars[prefix+".media.mimeType"] = input.MimeType
	vars[prefix+".media.sha256"] = input.MediaSHA256
	vars[prefix+".replyTo"] = input.ReplyTo
	vars[prefix+".forwarded"] = fmt.Sprint(input.Forwarded)
	vars[prefix+".frequentlyForwarded"] = fmt.Sprint(input.FrequentlyForwarded)
	vars[prefix+".reaction.emoji"] = input.ReactionEmoji
	vars[prefix+".reaction.targetMessageId"] = input.ReactionMessageID
	vars[prefix+".reaction.removed"] = fmt.Sprint(input.ReactionRemoved)
}

func evalRouter(cases []RouterCase, vars map[string]string) string {
	for _, routeCase := range cases {
		matched, err := evalExpression(routeCase.Expression, vars)
		if err == nil && matched {
			return routeCase.ID
		}
	}
	return "default"
}

// saveToolResult conserva la salida cruda por compatibilidad y proyecta los
// escalares de un objeto JSON a variables con punto. Así el grafo puede decidir
// por `receipt.decision` sin enseñarle al motor el esquema de cada integración.
// maxToolErrorRunes acota lo que puede acabar dentro de un mensaje al cliente.
// Un error de una API externa puede traer un cuerpo entero, y el flujo lo
// interpolaría tal cual en un WhatsApp.
const maxToolErrorRunes = 300

func toolErrorText(err error) string {
	text := strings.TrimSpace(err.Error())
	runes := []rune(text)
	if len(runes) > maxToolErrorRunes {
		return string(runes[:maxToolErrorRunes-1]) + "…"
	}
	return text
}

func saveToolResult(vars map[string]string, prefix, result string) {
	vars[prefix] = result
	var value any
	if json.Unmarshal([]byte(result), &value) != nil {
		return
	}
	flattenToolValue(vars, prefix, value, 0)
}

func saveAgentResult(vars map[string]string, prefix string, result AgentResult, fields []AgentOutputField) {
	vars[prefix+".branch"] = result.Branch
	if len(result.Data) == 0 || len(fields) == 0 {
		return
	}
	allowed := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		allowed[field.Key] = struct{}{}
	}
	for key, value := range result.Data {
		if _, ok := allowed[key]; ok {
			flattenToolValue(vars, prefix+"."+key, value, 0)
		}
	}
}

func flattenToolValue(vars map[string]string, prefix string, value any, depth int) {
	if depth > 4 {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key != "" {
				flattenToolValue(vars, prefix+"."+key, child, depth+1)
			}
		}
	case string:
		vars[prefix] = typed
	// json.Number llega desde la tool call del agente, que se decodifica con
	// UseNumber(); float64 llega desde el JSON de una herramienta. Omitir el
	// primero descartaba en silencio todo campo "number" del agente.
	case json.Number:
		vars[prefix] = typed.String()
	case float64, bool:
		vars[prefix] = fmt.Sprint(typed)
	case nil:
		vars[prefix] = ""
	}
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

func inputMatches(expect string, accepts []string, inputType string) bool {
	if len(accepts) > 0 {
		return containsInputType(accepts, inputType)
	}
	if expect == "" || expect == "any" {
		return true
	}
	if expect == "text" {
		return inputType == "text" || inputType == "reply" || inputType == "interactive"
	}
	return normalizedInputType(expect) == normalizedInputType(inputType)
}

func expectedInputPrompt(expect string, accepts []string) string {
	if len(accepts) == 1 {
		expect = accepts[0]
	}
	switch expect {
	case "image":
		return "Necesito que envíes una imagen para continuar."
	case "audio":
		return "Necesito que envíes un audio para continuar."
	case "document":
		return "Necesito que envíes un documento para continuar."
	case "video":
		return "Necesito que envíes un video para continuar."
	case "text":
		return "Necesito que respondas con un mensaje de texto."
	}
	if len(accepts) > 1 {
		return "Necesito que envíes uno de estos formatos para continuar: " + strings.Join(accepts, ", ") + "."
	}
	return "Necesito otro formato de mensaje para continuar."
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

// evalCondition conserva el contrato true/false del nodo antiguo, pero comparte
// el evaluador seguro del Router (sin eval, JavaScript ni SQL libre).
func evalCondition(expr string, vars map[string]string) string {
	matched, err := evalExpression(expr, vars)
	if err == nil && matched {
		return "true"
	}
	return "false"
}
