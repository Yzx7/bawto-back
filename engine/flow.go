// Package engine ejecuta el grafo de un flujo (trigger + nodos + aristas).
// Los tipos espejan frontend/lib/flow/types.ts (mismo shape que bots.flow JSONB).
package engine

import "strings"

type Trigger struct {
	Type     string   `json:"type"` // message | schedule | event
	Match    string   `json:"match,omitempty"`
	Keywords []string `json:"keywords,omitempty"`
	// Accepts filtra los tipos normalizados que pueden iniciar el flujo. Vacío
	// conserva el comportamiento legado: cualquier mensaje.
	Accepts []string `json:"accepts,omitempty"`
	// RouteBy vacío/"single" usa una salida; "content_type" usa un puerto por
	// tipo aceptado (text, image, audio, etc.).
	RouteBy  string `json:"routeBy,omitempty"`
	Cron     string `json:"cron,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	ViewID   string `json:"viewId,omitempty"`
	EventKey string `json:"eventKey,omitempty"`
	// ReplyIntent se inyecta como source_intent cuando una respuesta se
	// correlaciona con un run de este flujo. Cada negocio define sus propias
	// intenciones en la configuración del flujo.
	ReplyIntent string `json:"replyIntent,omitempty"`
}

type Node struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // send | agent | wait | tool | router | condition | action

	// send
	Body             string   `json:"body,omitempty"`
	TemplateName     string   `json:"templateName,omitempty"`
	TemplateLanguage string   `json:"templateLanguage,omitempty"`
	TemplateParams   []string `json:"templateParams,omitempty"`
	// agent
	Instruction  string             `json:"instruction,omitempty"`
	Outputs      []string           `json:"outputs,omitempty"`
	OutputFields []AgentOutputField `json:"outputFields,omitempty"`
	AgentRef     string             `json:"agentRef,omitempty"`
	// Tools son las herramientas que el modelo puede llamar durante el turno.
	// Van en el nodo y no en el bot a propósito: la versión publicada del flujo es
	// inmutable, así que queda registrado qué podía hacer el agente en el momento
	// en que dijo lo que dijo. Colgarlas del bot cambiaría el comportamiento de
	// todos los flujos publicados sin dejar rastro.
	Tools []NodeTool `json:"tools,omitempty"`
	// ContextMode controla si este agente recibe el historial del flujo.
	// Vacío y "none" usan únicamente el mensaje/variables actuales.
	ContextMode string `json:"contextMode,omitempty"` // none | recent
	// Silent: el agente solo decide la rama y no le escribe al cliente. Evita el
	// mensaje duplicado cuando la rama ya termina en un `send` con el texto oficial.
	Silent bool `json:"silent,omitempty"`
	// wait
	Expect string `json:"expect,omitempty"`
	// Accepts declara formatos adicionales en agent y conserva el contrato tipado
	// legado de wait. Un agent vacío acepta texto/interacción; incluir image hace
	// explícito que el proveedor debe recibir la captura actual.
	Accepts      []string `json:"accepts,omitempty"`
	SaveAs       string   `json:"saveAs,omitempty"`
	TimeoutHours int      `json:"timeoutHours,omitempty"`
	// tool
	ToolRef string            `json:"toolRef,omitempty"`
	Args    map[string]string `json:"args,omitempty"`
	// condition
	Expression string `json:"expression,omitempty"`
	// router
	// Cases se evalúa en orden y gana el primer caso verdadero. Si ninguno
	// coincide, el motor continúa por la salida reservada "default".
	Cases []RouterCase `json:"cases,omitempty"`
	// action
	Action string            `json:"action,omitempty"`
	Params map[string]string `json:"params,omitempty"`
}

// AgentOutputField declara un dato que el modelo puede extraer además de la
// rama obligatoria. Todos los campos son opcionales en cada ejecución: una rama
// de revisión puede no conocer todavía el monto, pero nunca puede omitir la
// elección de la siguiente conexión.
type AgentOutputField struct {
	Key         string `json:"key"`
	Type        string `json:"type"` // string | number | boolean | datetime
	Description string `json:"description,omitempty"`
}

// RouterCase describe una rama ordenada de un Router. ID es el handle estable
// que usan las aristas; Label es solo presentación y puede cambiar sin romper
// conexiones.
type RouterCase struct {
	ID         string `json:"id"`
	Label      string `json:"label,omitempty"`
	Expression string `json:"expression"`
}

// NodeTool es una herramienta habilitada en un nodo `agent`.
//
// `Config` es lo que fija el autor y el modelo no puede elegir —por ejemplo en
// qué tabla busca—. Los argumentos de la llamada los redacta el modelo según el
// esquema de la herramienta; aquí solo se acota su alcance.
type NodeTool struct {
	Ref    string            `json:"ref"`
	Config map[string]string `json:"config,omitempty"`
}

type Edge struct {
	ID           string `json:"id"`
	Source       string `json:"source"`
	Target       string `json:"target"`
	SourceHandle string `json:"sourceHandle,omitempty"`
	// Role es semántica explícita del editor; el motor sigue source → target.
	Role string `json:"role,omitempty"` // loopback
	// Tag es presentación del editor: la conexión se dibuja como una etiqueta
	// con nombre en ambos extremos en vez de como una línea. El motor la ignora;
	// está aquí para que el campo no se pierda si el grafo se reescribe desde
	// esta struct.
	Tag string `json:"tag,omitempty"`
}

type Flow struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Trigger Trigger `json:"trigger"`
	Nodes   []Node  `json:"nodes"`
	Edges   []Edge  `json:"edges"`
}

// TriggerMatches decide si un mensaje nuevo inicia el flujo. Las conversaciones
// que ya están en un wait se reanudan independientemente de este filtro.
func TriggerMatches(trigger Trigger, input string) bool {
	return TriggerMatchesInput(trigger, input, "text")
}

// TriggerMatchesInput añade el filtro de formato sin romper llamadores que
// todavía solo simulan texto mediante TriggerMatches.
func TriggerMatchesInput(trigger Trigger, input, inputType string) bool {
	if trigger.Type != "message" {
		return false
	}
	if len(trigger.Accepts) > 0 && !containsInputType(trigger.Accepts, inputType) {
		return false
	}
	if trigger.Match != "keyword" {
		return true
	}
	input = strings.ToLower(input)
	for _, keyword := range trigger.Keywords {
		if keyword = strings.TrimSpace(strings.ToLower(keyword)); keyword != "" && strings.Contains(input, keyword) {
			return true
		}
	}
	return false
}

// start devuelve la primera rama real. Un trigger tipado no puede caer a una
// salida cualquiera si falta el puerto: eso escondería un grafo incompleto.
func (f *Flow) start(inputType string) string {
	if f.Trigger.RouteBy == "content_type" {
		handle := normalizedInputType(inputType)
		for _, edge := range f.Edges {
			if edge.Source == "trigger" && edge.SourceHandle == handle {
				return edge.Target
			}
		}
		return ""
	}
	return f.next("trigger", "")
}

func normalizedInputType(inputType string) string {
	if inputType == "reply" {
		return "interactive"
	}
	return inputType
}

func containsInputType(types []string, inputType string) bool {
	want := normalizedInputType(inputType)
	for _, item := range types {
		if item == "any" || normalizedInputType(item) == want {
			return true
		}
	}
	return false
}

// node devuelve el nodo por id (nil si no existe).
func (f *Flow) node(id string) *Node {
	for i := range f.Nodes {
		if f.Nodes[i].ID == id {
			return &f.Nodes[i]
		}
	}
	return nil
}

// next devuelve el id del nodo destino desde `from` por el handle (rama) dado.
// Con handle != "" prefiere la arista de esa rama exacta; si no hay, cae a una
// arista sin handle; si tampoco, a cualquiera. "" si no hay ninguna.
func (f *Flow) next(from, handle string) string {
	var anyEdge, emptyHandle string
	for _, e := range f.Edges {
		if e.Source != from {
			continue
		}
		if handle != "" && e.SourceHandle == handle {
			return e.Target
		}
		if anyEdge == "" {
			anyEdge = e.Target
		}
		if e.SourceHandle == "" && emptyHandle == "" {
			emptyHandle = e.Target
		}
	}
	if emptyHandle != "" {
		return emptyHandle
	}
	return anyEdge
}
