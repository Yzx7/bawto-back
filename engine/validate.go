package engine

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Yzx7/sacs-chatbots/engine/tools"
)

var branchNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
var variableNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// Validate comprueba el contrato que el motor realmente puede ejecutar. Evita
// publicar grafos que el editor puede dibujar pero el backend no sabe resolver.
func Validate(flow *Flow) error {
	if flow == nil || strings.TrimSpace(flow.ID) == "" || strings.TrimSpace(flow.Name) == "" {
		return fmt.Errorf("el flujo requiere id y nombre")
	}
	if len(flow.Nodes) == 0 {
		return fmt.Errorf("el flujo requiere al menos un nodo")
	}
	if len(flow.Nodes) > 200 {
		return fmt.Errorf("el flujo supera el máximo de 200 nodos")
	}
	switch flow.Trigger.Type {
	case "message":
		if flow.Trigger.Match == "keyword" && len(flow.Trigger.Keywords) == 0 {
			return fmt.Errorf("el trigger por palabras clave requiere al menos una palabra")
		}
		if flow.Trigger.RouteBy != "" && flow.Trigger.RouteBy != "single" && flow.Trigger.RouteBy != "content_type" {
			return fmt.Errorf("modo de salida del trigger inválido")
		}
		if err := validateInputTypes(flow.Trigger.Accepts); err != nil {
			return fmt.Errorf("trigger: %w", err)
		}
		if flow.Trigger.RouteBy == "content_type" && len(flow.Trigger.Accepts) == 0 {
			return fmt.Errorf("el trigger separado por formato requiere al menos un tipo aceptado")
		}
	case "schedule":
		if strings.TrimSpace(flow.Trigger.Cron) == "" || strings.TrimSpace(flow.Trigger.ViewID) == "" {
			return fmt.Errorf("el trigger programado requiere cron y vista de datos")
		}
	case "event":
		return fmt.Errorf("los triggers event aún no tienen receptor implementado")
	default:
		return fmt.Errorf("tipo de trigger inválido")
	}

	nodes := map[string]*Node{}
	for i := range flow.Nodes {
		n := &flow.Nodes[i]
		if n.ID == "" || n.ID == "trigger" {
			return fmt.Errorf("nodo %d tiene un id inválido", i+1)
		}
		if nodes[n.ID] != nil {
			return fmt.Errorf("id de nodo duplicado: %s", n.ID)
		}
		nodes[n.ID] = n
		if err := validateNode(n, flow.Trigger.Type); err != nil {
			return fmt.Errorf("nodo %s: %w", n.ID, err)
		}
	}

	out := map[string]map[string]int{}
	for _, edge := range flow.Edges {
		if edge.Source != "trigger" && nodes[edge.Source] == nil {
			return fmt.Errorf("arista %s parte de un nodo inexistente", edge.ID)
		}
		if nodes[edge.Target] == nil {
			return fmt.Errorf("arista %s apunta a un nodo inexistente", edge.ID)
		}
		if edge.Role != "" && edge.Role != "loopback" {
			return fmt.Errorf("arista %s tiene rol %q inválido", edge.ID, edge.Role)
		}
		if out[edge.Source] == nil {
			out[edge.Source] = map[string]int{}
		}
		out[edge.Source][edge.SourceHandle]++
		if out[edge.Source][edge.SourceHandle] > 1 {
			return fmt.Errorf("%s tiene más de una salida para la rama %q", edge.Source, edge.SourceHandle)
		}
	}
	if flow.Trigger.RouteBy == "content_type" {
		allowed := make(map[string]struct{}, len(flow.Trigger.Accepts))
		for _, inputType := range flow.Trigger.Accepts {
			handle := normalizedInputType(inputType)
			allowed[handle] = struct{}{}
			if out["trigger"][handle] != 1 {
				return fmt.Errorf("el trigger requiere una salida %q", handle)
			}
		}
		for handle := range out["trigger"] {
			if _, ok := allowed[handle]; !ok {
				return fmt.Errorf("el trigger tiene una conexión para el formato no declarado %q", handle)
			}
		}
	} else if outgoingCount(out["trigger"]) != 1 {
		return fmt.Errorf("el trigger debe tener exactamente una salida")
	}
	for _, n := range flow.Nodes {
		if isLinearNode(&n) && !(n.Kind == "wait" && legacyTypedWait(&n) && len(n.Accepts) > 1) && outgoingCount(out[n.ID]) != 1 {
			return fmt.Errorf("el nodo %s debe tener exactamente una salida", n.ID)
		}
		required := requiredHandles(&n)
		for _, handle := range required {
			if out[n.ID][handle] != 1 {
				return fmt.Errorf("el nodo %s requiere una salida %q", n.ID, handle)
			}
		}
		if n.Kind == "agent" || n.Kind == "router" {
			allowedHandles := requiredHandles(&n)
			allowed := make(map[string]struct{}, len(allowedHandles))
			for _, handle := range allowedHandles {
				allowed[handle] = struct{}{}
			}
			for handle := range out[n.ID] {
				if _, exists := allowed[handle]; !exists {
					return fmt.Errorf("el nodo %s tiene una conexión para la rama no declarada %q", n.ID, handle)
				}
			}
		}
	}
	for _, edge := range flow.Edges {
		if inputType := edgeInputType(flow, nodes, edge); inputType != "" {
			if err := validateInputDestination(inputType, nodes[edge.Target]); err != nil {
				return fmt.Errorf("conexión %s → %s: %w", edge.Source, edge.Target, err)
			}
		}
		if edge.Source != "trigger" && edge.SourceHandle == "ok" {
			source := nodes[edge.Source]
			if source != nil && source.Kind == "tool" {
				if err := validatePayloadDestination(tools.Get(source.ToolRef).Produces, nodes[edge.Target]); err != nil {
					return fmt.Errorf("conexión %s → %s: %w", edge.Source, edge.Target, err)
				}
			}
		}
	}

	seen := map[string]bool{}
	queue := make([]string, 0, len(flow.Edges))
	for _, edge := range flow.Edges {
		if edge.Source == "trigger" {
			queue = append(queue, edge.Target)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		for _, edge := range flow.Edges {
			if edge.Source == id {
				queue = append(queue, edge.Target)
			}
		}
	}
	for id := range nodes {
		if !seen[id] {
			return fmt.Errorf("el nodo %s no es alcanzable desde el trigger", id)
		}
	}
	for edgeIndex, edge := range flow.Edges {
		if edge.Role == "loopback" && edge.Source != edge.Target &&
			!pathExists(flow.Edges, edge.Target, edge.Source, edgeIndex) {
			return fmt.Errorf("la arista %s está marcada como loopback pero no cierra un ciclo", edge.ID)
		}
	}
	if cycle := automaticCycleWithoutWait(flow.Nodes, flow.Edges, nodes); len(cycle) > 0 {
		return fmt.Errorf("el flujo contiene un ciclo sin un nodo wait entre los nodos: %s", strings.Join(cycle, ", "))
	}
	return nil
}

// ValidateStructural valida la integridad de cada nodo y arista sin exigir
// que el grafo esté completo ni totalmente conectado (usado en mutaciones intermedias de autoría).
func ValidateStructural(flow *Flow) error {
	if flow == nil || strings.TrimSpace(flow.ID) == "" || strings.TrimSpace(flow.Name) == "" {
		return fmt.Errorf("el flujo requiere id y nombre")
	}
	if len(flow.Nodes) > 200 {
		return fmt.Errorf("el flujo supera el máximo de 200 nodos")
	}
	switch flow.Trigger.Type {
	case "message":
		if flow.Trigger.Match == "keyword" && len(flow.Trigger.Keywords) == 0 {
			return fmt.Errorf("el trigger por palabras clave requiere al menos una palabra")
		}
		if flow.Trigger.RouteBy != "" && flow.Trigger.RouteBy != "single" && flow.Trigger.RouteBy != "content_type" {
			return fmt.Errorf("modo de salida del trigger inválido")
		}
		if err := validateInputTypes(flow.Trigger.Accepts); err != nil {
			return fmt.Errorf("trigger: %w", err)
		}
	case "schedule":
		if strings.TrimSpace(flow.Trigger.Cron) == "" || strings.TrimSpace(flow.Trigger.ViewID) == "" {
			return fmt.Errorf("el trigger programado requiere cron y vista de datos")
		}
	case "event":
		return fmt.Errorf("los triggers event aún no tienen receptor implementado")
	default:
		return fmt.Errorf("tipo de trigger inválido")
	}

	nodes := map[string]*Node{}
	for i := range flow.Nodes {
		n := &flow.Nodes[i]
		if n.ID == "" || n.ID == "trigger" {
			return fmt.Errorf("nodo %d tiene un id inválido", i+1)
		}
		if nodes[n.ID] != nil {
			return fmt.Errorf("id de nodo duplicado: %s", n.ID)
		}
		nodes[n.ID] = n
		if err := validateNode(n, flow.Trigger.Type); err != nil {
			return fmt.Errorf("nodo %s: %w", n.ID, err)
		}
	}

	out := map[string]map[string]int{}
	for _, edge := range flow.Edges {
		if edge.Source != "trigger" && nodes[edge.Source] == nil {
			return fmt.Errorf("arista %s parte de un nodo inexistente", edge.ID)
		}
		if nodes[edge.Target] == nil {
			return fmt.Errorf("arista %s apunta a un nodo inexistente", edge.ID)
		}
		if edge.Role != "" && edge.Role != "loopback" {
			return fmt.Errorf("arista %s tiene rol %q inválido", edge.ID, edge.Role)
		}
		if out[edge.Source] == nil {
			out[edge.Source] = map[string]int{}
		}
		out[edge.Source][edge.SourceHandle]++
		if out[edge.Source][edge.SourceHandle] > 1 {
			return fmt.Errorf("%s tiene más de una salida para la rama %q", edge.Source, edge.SourceHandle)
		}
	}
	for _, edge := range flow.Edges {
		if inputType := edgeInputType(flow, nodes, edge); inputType != "" {
			if err := validateInputDestination(inputType, nodes[edge.Target]); err != nil {
				return fmt.Errorf("conexión %s → %s: %w", edge.Source, edge.Target, err)
			}
		}
		if edge.Source != "trigger" && edge.SourceHandle == "ok" {
			source := nodes[edge.Source]
			if source != nil && source.Kind == "tool" {
				if err := validatePayloadDestination(tools.Get(source.ToolRef).Produces, nodes[edge.Target]); err != nil {
					return fmt.Errorf("conexión %s → %s: %w", edge.Source, edge.Target, err)
				}
			}
		}
	}
	if cycle := automaticCycleWithoutWait(flow.Nodes, flow.Edges, nodes); len(cycle) > 0 {
		return fmt.Errorf("el flujo contiene un ciclo sin un nodo wait entre los nodos: %s", strings.Join(cycle, ", "))
	}
	return nil
}

func edgeInputType(flow *Flow, nodes map[string]*Node, edge Edge) string {
	if edge.Source == "trigger" && flow.Trigger.RouteBy == "content_type" {
		return normalizedInputType(edge.SourceHandle)
	}
	source := nodes[edge.Source]
	if source == nil || source.Kind != "wait" || !legacyTypedWait(source) {
		return ""
	}
	if len(source.Accepts) > 1 {
		return normalizedInputType(edge.SourceHandle)
	}
	if len(source.Accepts) == 1 {
		return normalizedInputType(source.Accepts[0])
	}
	if source.Expect != "" && source.Expect != "any" {
		return normalizedInputType(source.Expect)
	}
	return ""
}

func validateInputDestination(inputType string, destination *Node) error {
	if destination == nil {
		return nil
	}
	if destination.Kind == "agent" && !agentAcceptsInput(destination, inputType) {
		return fmt.Errorf("el agente no declara entrada %s; habilita ese formato o inserta una herramienta de transformación", inputType)
	}
	if destination.Kind != "tool" {
		return nil
	}
	payload := tools.PayloadType(inputType)
	if !toolAccepts(tools.Get(destination.ToolRef), payload) {
		return fmt.Errorf("la herramienta %q no acepta %s", destination.ToolRef, inputType)
	}
	return nil
}

func validatePayloadDestination(payload tools.PayloadType, destination *Node) error {
	if destination == nil || destination.Kind != "tool" || payload == "" {
		return nil
	}
	if !toolAccepts(tools.Get(destination.ToolRef), payload) {
		return fmt.Errorf("la herramienta %q no acepta el resultado %s", destination.ToolRef, payload)
	}
	return nil
}

func toolAccepts(spec *tools.Spec, payload tools.PayloadType) bool {
	if spec == nil {
		return false
	}
	for _, accepted := range spec.Accepts {
		if accepted == payload {
			return true
		}
	}
	return false
}

func validateNode(n *Node, triggerType string) error {
	switch n.Kind {
	case "send":
		if n.Body == "" && n.TemplateName == "" {
			return fmt.Errorf("requiere texto o plantilla")
		}
		if n.TemplateName != "" && strings.TrimSpace(n.TemplateLanguage) == "" {
			return fmt.Errorf("la plantilla requiere idioma")
		}
		if triggerType == "schedule" && n.TemplateName == "" {
			return fmt.Errorf("un flujo programado solo puede enviar plantillas")
		}
	case "agent":
		if triggerType == "schedule" {
			return fmt.Errorf("los agentes no están disponibles en flujos programados")
		}
		if strings.TrimSpace(n.Instruction) == "" || len(n.Outputs) == 0 {
			return fmt.Errorf("requiere instrucción y ramas")
		}
		if n.AgentRef != "" {
			return fmt.Errorf("agentRef aún no está implementado")
		}
		if n.AgentRole != "" && n.AgentRole != "specialist" && n.AgentRole != "orchestrator" {
			return fmt.Errorf("agentRole %q inválido", n.AgentRole)
		}
		if n.AgentRole == "orchestrator" && len(n.Tools) > 0 {
			return fmt.Errorf("un orquestador no puede tener herramientas; solo clasifica y enruta")
		}
		if n.ContextMode != "" && n.ContextMode != "none" && n.ContextMode != "recent" {
			return fmt.Errorf("contextMode %q inválido", n.ContextMode)
		}
		if n.ContextMode == "recent" && strings.Contains(n.Instruction, "{input}") {
			return fmt.Errorf("contextMode recent ya recibe el mensaje actual en el historial; no uses {input} en la instrucción")
		}
		for _, inputType := range n.Accepts {
			if inputType != "text" && inputType != "interactive" && inputType != "image" {
				return fmt.Errorf("el agente todavía no admite entrada %q", inputType)
			}
		}
		if err := validateInputTypes(n.Accepts); err != nil {
			return err
		}
		if err := validateAgentTools(n.Tools); err != nil {
			return err
		}
		if err := validateAgentOutput(n); err != nil {
			return err
		}
		seenOutputs := make(map[string]struct{}, len(n.Outputs))
		outputNames := make(map[string]struct{}, len(n.Outputs))
		for _, output := range n.Outputs {
			if !branchNameRe.MatchString(output) {
				return fmt.Errorf("rama %q inválida: usa 1–64 letras ASCII, números, guion o guion bajo", output)
			}
			key := strings.ToLower(output)
			if _, exists := seenOutputs[key]; exists {
				return fmt.Errorf("rama duplicada %q", output)
			}
			seenOutputs[key] = struct{}{}
			outputNames[output] = struct{}{}
		}
		if n.Silent && len(n.ReplyOn) > 0 {
			return fmt.Errorf("un agente silencioso no puede declarar ramas de respuesta")
		}
		if n.AgentRole == "orchestrator" && !n.Silent && len(n.ReplyOn) == 0 {
			return fmt.Errorf("un orquestador que habla debe limitar su respuesta a ramas concretas")
		}
		seenReplyOn := make(map[string]struct{}, len(n.ReplyOn))
		for _, branch := range n.ReplyOn {
			if _, exists := outputNames[branch]; !exists {
				return fmt.Errorf("la rama de respuesta %q no existe en outputs", branch)
			}
			key := strings.ToLower(branch)
			if _, exists := seenReplyOn[key]; exists {
				return fmt.Errorf("rama de respuesta duplicada %q", branch)
			}
			seenReplyOn[key] = struct{}{}
		}
	case "wait":
		if triggerType != "message" {
			return fmt.Errorf("solo se puede esperar en flujos de mensajes")
		}
		if len(n.Accepts) > 0 {
			if err := validateInputTypes(n.Accepts); err != nil {
				return err
			}
		} else if n.Expect != "" && n.Expect != "any" && n.Expect != "text" && n.Expect != "image" {
			return fmt.Errorf("tipo de espera inválido")
		}
	case "tool":
		spec := tools.Get(n.ToolRef)
		if spec == nil {
			return fmt.Errorf("herramienta %q no implementada", n.ToolRef)
		}
		if !spec.ForGraph {
			return fmt.Errorf("la herramienta %q solo puede usarla un agente, no un bloque del grafo", n.ToolRef)
		}
		if len(n.Tools) > 0 {
			return fmt.Errorf("`tools` es del bloque agente; este ejecuta una sola herramienta con `toolRef`")
		}
		switch n.ToolRef {
		case "data_mutate":
			if err := validateDataMutateArgs(n.Args); err != nil {
				return err
			}
		case "data_query":
			if err := validateDataQueryArgs(n.Args); err != nil {
				return err
			}
		case "catalog_search":
			if err := validateCatalogSearchArgs(n.Args); err != nil {
				return err
			}
		case "catalog_product":
			if err := validateCatalogProductArgs(n.Args); err != nil {
				return err
			}
		case "order_create":
			if err := validateOrderCreateArgs(n.Args); err != nil {
				return err
			}
		case "payment_intent_create":
			if err := validatePaymentIntentArgs(n.Args); err != nil {
				return err
			}
		case "payment_submit":
			if err := validatePaymentSubmitArgs(n.Args); err != nil {
				return err
			}
		case "payment_methods_render":
			if len(n.Args) != 0 {
				return fmt.Errorf("payment_methods_render no admite argumentos")
			}
		case "subscription_activate":
			if err := validateSubscriptionActivateArgs(n.Args); err != nil {
				return err
			}
		case "credit_recharge_activate":
			if err := validateCreditRechargeActivateArgs(n.Args); err != nil {
				return err
			}
		}
	case "condition":
		if err := validateExpression(n.Expression); err != nil {
			return fmt.Errorf("expresión inválida: %w", err)
		}
	case "router":
		if len(n.Cases) == 0 {
			return fmt.Errorf("requiere al menos un caso además de default")
		}
		if len(n.Cases) > 32 {
			return fmt.Errorf("supera el máximo de 32 casos")
		}
		seenCases := make(map[string]struct{}, len(n.Cases))
		for index, routeCase := range n.Cases {
			if !branchNameRe.MatchString(routeCase.ID) || strings.EqualFold(routeCase.ID, "default") {
				return fmt.Errorf("caso %d tiene id %q inválido", index+1, routeCase.ID)
			}
			key := strings.ToLower(routeCase.ID)
			if _, exists := seenCases[key]; exists {
				return fmt.Errorf("caso duplicado %q", routeCase.ID)
			}
			seenCases[key] = struct{}{}
			if err := validateExpression(routeCase.Expression); err != nil {
				return fmt.Errorf("caso %q: %w", routeCase.ID, err)
			}
		}
	case "action":
		if n.Action != "set" && n.Action != "handoff" && n.Action != "end" {
			return fmt.Errorf("acción %q no implementada", n.Action)
		}
	default:
		return fmt.Errorf("tipo %q inválido", n.Kind)
	}
	return nil
}

func agentAcceptsInput(agent *Node, inputType string) bool {
	if agent == nil || agent.Kind != "agent" {
		return false
	}
	if len(agent.Accepts) == 0 {
		return inputType == "text" || inputType == "interactive"
	}
	return containsInputType(agent.Accepts, inputType)
}

func validateAgentOutput(n *Node) error {
	if n.SaveAs != "" && !variableNameRe.MatchString(n.SaveAs) {
		return fmt.Errorf("guardar datos como %q es inválido: usa una variable de 1–64 letras ASCII, números o guion bajo", n.SaveAs)
	}
	if len(n.OutputFields) == 0 {
		return nil
	}
	if n.SaveAs == "" {
		return fmt.Errorf("los datos de salida requieren `saveAs`")
	}
	if len(n.OutputFields) > 32 {
		return fmt.Errorf("los datos de salida superan el máximo de 32 campos")
	}
	seen := make(map[string]struct{}, len(n.OutputFields))
	for index, field := range n.OutputFields {
		if !variableNameRe.MatchString(field.Key) || strings.EqualFold(field.Key, "branch") {
			return fmt.Errorf("campo de salida %d tiene clave %q inválida o reservada", index+1, field.Key)
		}
		key := strings.ToLower(field.Key)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("campo de salida duplicado %q", field.Key)
		}
		seen[key] = struct{}{}
		switch field.Type {
		case "string", "number", "boolean", "datetime":
		default:
			return fmt.Errorf("campo de salida %q tiene tipo %q inválido", field.Key, field.Type)
		}
		if len([]rune(field.Description)) > 500 {
			return fmt.Errorf("la descripción del campo de salida %q supera 500 caracteres", field.Key)
		}
	}
	return nil
}

// dataQueryRuleRe reconoce `where.<n>.<parte>` con n de 1 a 8. El índice se
// limita aquí y no en el ejecutor porque un flujo publicado ya no debe poder
// crecer sin pasar por el validador.
var dataQueryRuleRe = regexp.MustCompile(`^where\.([1-8])\.(field|op|value)$`)

// validateDataQueryArgs distingue lo que el autor fija de lo que el runtime
// interpola. `object`, `fields`, `orderBy` y cada `where.<n>.field`/`op` no
// pueden contener `{`: si el mensaje del cliente pudiera elegir tabla, campo u
// operador, el aislamiento por organización sería lo único que quedaría en pie.
// Los `value` sí son interpolables; ahí está toda la utilidad del bloque.
func validateDataQueryArgs(args map[string]string) error {
	object := strings.TrimSpace(args["object"])
	if object == "" || strings.Contains(object, "{") {
		return fmt.Errorf("data_query requiere un objeto fijo elegido por el autor")
	}
	for _, key := range []string{"fields", "orderBy"} {
		if strings.Contains(args[key], "{") {
			return fmt.Errorf("data_query no admite variables en %s", key)
		}
	}
	if dir := strings.TrimSpace(args["orderDir"]); dir != "" && dir != "asc" && dir != "desc" {
		return fmt.Errorf("data_query orderDir debe ser asc o desc")
	}
	if raw := strings.TrimSpace(args["limit"]); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 50 {
			return fmt.Errorf("data_query limit debe ser un número entre 1 y 50")
		}
	}
	if raw := strings.TrimSpace(args["linkCurrentContact"]); raw != "" {
		if _, err := strconv.ParseBool(raw); err != nil {
			return fmt.Errorf("data_query linkCurrentContact debe ser true o false")
		}
	}

	allowed := map[string]bool{
		"object": true, "fields": true, "orderBy": true, "orderDir": true,
		"limit": true, "linkCurrentContact": true,
	}
	rules := map[string]bool{}
	for key := range args {
		if allowed[key] {
			continue
		}
		match := dataQueryRuleRe.FindStringSubmatch(key)
		if match == nil {
			return fmt.Errorf("data_query no admite el argumento %q", key)
		}
		if match[2] == "field" {
			rules[match[1]] = true
		}
	}
	for key, value := range args {
		match := dataQueryRuleRe.FindStringSubmatch(key)
		if match == nil || match[2] == "value" {
			continue
		}
		if strings.TrimSpace(value) == "" || strings.Contains(value, "{") {
			return fmt.Errorf("data_query requiere un %s fijo en la condición %s", match[2], match[1])
		}
		if match[2] == "op" && !validDataQueryOp(value) {
			return fmt.Errorf("data_query no admite el operador %q", value)
		}
		if !rules[match[1]] {
			return fmt.Errorf("la condición %s de data_query no declara campo", match[1])
		}
	}
	return nil
}

// validDataQueryOp espeja models.DataQueryOperators. Se duplica la lista porque
// `engine` no puede importar `models` —es `models` quien importa `engine`—; la
// prueba de coherencia vive del lado que sí ve ambos paquetes.
func validDataQueryOp(op string) bool {
	switch strings.TrimSpace(op) {
	case "eq", "neq", "lt", "lte", "gt", "gte", "contains", "in", "exists":
		return true
	}
	return false
}

// maxCatalogResults es el tope duro de productos que vuelven en una llamada.
//
// No es una preferencia estética: cada resultado se reenvía al modelo en todas
// las iteraciones siguientes del turno, así que un catálogo entero encarece el
// bucle completo, no solo el paso que lo pidió. Además, la respuesta se le lee a
// alguien por WhatsApp: diez productos ya no se leen.
const maxCatalogResults = 10

// validateCatalogSearchArgs comprueba el bloque del grafo.
//
// La asimetría es la misma que en data_query y es el contrato: **solo `query`
// interpola variables**. La conexión, la categoría y el tope los fija el autor,
// así que llevan `{` prohibido. Sin esa asimetría, el texto del cliente podría
// elegir a qué tienda se pregunta.
func validateCatalogSearchArgs(args map[string]string) error {
	if err := requireFixedConnection(args, "catalog_search"); err != nil {
		return err
	}
	allowed := map[string]bool{
		"connection": true, "query": true, "categoryId": true, "includeDescendants": true,
		"minPrice": true, "maxPrice": true, "sort": true, "limit": true, "urlTemplate": true,
	}
	for key := range args {
		if !allowed[key] {
			return fmt.Errorf("catalog_search no admite el argumento %q", key)
		}
	}
	for _, key := range []string{"categoryId", "includeDescendants", "sort", "limit"} {
		if strings.Contains(args[key], "{") {
			return fmt.Errorf("catalog_search requiere un %s fijo, sin variables", key)
		}
	}
	if raw := strings.TrimSpace(args["categoryId"]); raw != "" {
		if id, err := strconv.Atoi(raw); err != nil || id <= 0 {
			return fmt.Errorf("catalog_search requiere una categoría numérica")
		}
	}
	if raw := strings.TrimSpace(args["includeDescendants"]); raw != "" {
		if _, err := strconv.ParseBool(raw); err != nil {
			return fmt.Errorf("includeDescendants debe ser true o false")
		}
	}
	if raw := strings.TrimSpace(args["limit"]); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit <= 0 {
			return fmt.Errorf("catalog_search requiere un limit positivo")
		}
		if limit > maxCatalogResults {
			return fmt.Errorf("catalog_search admite como mucho %d productos por consulta", maxCatalogResults)
		}
	}
	if sort := strings.TrimSpace(args["sort"]); sort != "" && !validCatalogSort(sort) {
		return fmt.Errorf("catalog_search no admite el orden %q", sort)
	}
	return validateURLTemplate(args["urlTemplate"])
}

func validateCatalogProductArgs(args map[string]string) error {
	if err := requireFixedConnection(args, "catalog_product"); err != nil {
		return err
	}
	allowed := map[string]bool{"connection": true, "productId": true, "slug": true, "urlTemplate": true}
	for key := range args {
		if !allowed[key] {
			return fmt.Errorf("catalog_product no admite el argumento %q", key)
		}
	}
	productID := strings.TrimSpace(args["productId"])
	slug := strings.TrimSpace(args["slug"])
	if (productID == "") == (slug == "") {
		return fmt.Errorf("catalog_product requiere productId o slug, pero no ambos")
	}
	return validateURLTemplate(args["urlTemplate"])
}

// requireFixedConnection impide que el destino de la llamada dependa de una
// variable. Es la barrera que mantiene fuera del alcance del cliente la
// pregunta «¿a qué tienda se consulta?».
func requireFixedConnection(args map[string]string, tool string) error {
	connection := strings.TrimSpace(args["connection"])
	if connection == "" {
		return fmt.Errorf("%s requiere una conexión elegida por el autor", tool)
	}
	if strings.Contains(connection, "{") {
		return fmt.Errorf("%s requiere una conexión fija, no una variable", tool)
	}
	return nil
}

// validateURLTemplate acota la plantilla del enlace a un https con un único
// hueco conocido. Sin esto sería un sitio cómodo para colar cualquier URL en un
// mensaje que el cliente va a tocar.
func validateURLTemplate(template string) error {
	template = strings.TrimSpace(template)
	if template == "" {
		return nil
	}
	if !strings.HasPrefix(template, "https://") {
		return fmt.Errorf("la plantilla del enlace debe empezar por https://")
	}
	for _, match := range varRe.FindAllStringSubmatch(template, -1) {
		if match[1] != "slug" && match[1] != "id" {
			return fmt.Errorf("la plantilla del enlace solo admite {slug} o {id}, no %q", match[1])
		}
	}
	return nil
}

func validCatalogSort(sort string) bool {
	switch sort {
	case "price-asc", "price-desc", "name-asc", "name-desc", "newest", "oldest":
		return true
	}
	return false
}

// maxOrderLines acota las líneas de un pedido. No es un límite de negocio: es
// que un carrito armado por conversación con más de diez líneas distintas ya no
// se pudo confirmar leyéndoselo a nadie por WhatsApp.
const maxOrderLines = 10

var orderLineRe = regexp.MustCompile(`^item\.([1-9]|10)\.(productId|quantity|variantId)$`)

// validateOrderCreateArgs comprueba el bloque que cierra la venta.
//
// La clave de idempotencia es obligatoria, igual que en data_mutate y por la
// misma razón elevada al cuadrado: aquí un reintento no duplica una fila, duplica
// un pedido con mercadería y dinero detrás.
func validateOrderCreateArgs(args map[string]string) error {
	if err := requireFixedConnection(args, "order_create"); err != nil {
		return err
	}
	if strings.TrimSpace(args["customerEmail"]) == "" {
		return fmt.Errorf("order_create requiere el correo del comprador, que es lo único que la tienda exige")
	}
	if strings.TrimSpace(args["idempotencyKey"]) == "" {
		return fmt.Errorf("order_create requiere idempotencyKey: sin ella un webhook reintentado crea dos pedidos")
	}
	allowed := map[string]bool{
		"connection": true, "customerEmail": true, "customerName": true, "customerPhone": true,
		"notes": true, "couponCode": true, "currency": true, "idempotencyKey": true,
		"shipping.name": true, "shipping.phone": true, "shipping.address": true,
		"shipping.city": true, "shipping.state": true, "shipping.postalCode": true,
		"shipping.country": true, "shipping.reference": true,
	}
	lines := map[string]bool{}
	for key := range args {
		if allowed[key] {
			continue
		}
		match := orderLineRe.FindStringSubmatch(key)
		if match == nil {
			return fmt.Errorf("order_create no admite el argumento %q", key)
		}
		if match[2] == "productId" {
			lines[match[1]] = true
		}
	}
	if len(lines) == 0 {
		return fmt.Errorf("order_create requiere al menos una línea con item.1.productId")
	}
	if len(lines) > maxOrderLines {
		return fmt.Errorf("order_create admite como mucho %d líneas", maxOrderLines)
	}
	for key := range args {
		match := orderLineRe.FindStringSubmatch(key)
		if match == nil || match[2] == "productId" {
			continue
		}
		if !lines[match[1]] {
			return fmt.Errorf("la línea %s de order_create no declara producto", match[1])
		}
	}
	for index := range lines {
		if strings.TrimSpace(args["item."+index+".quantity"]) == "" {
			return fmt.Errorf("la línea %s de order_create no declara cantidad", index)
		}
	}
	// El precio no es un argumento y no debe llegar a serlo: la tienda lo lee de
	// su catálogo dentro de la transacción y descarta cualquiera que se le mande.
	// Aceptarlo aquí haría creer que el flujo controla el importe.
	return nil
}

func validatePaymentIntentArgs(args map[string]string) error {
	if err := requireFixedConnection(args, "payment_intent_create"); err != nil {
		return err
	}
	if strings.TrimSpace(args["orderId"]) == "" {
		return fmt.Errorf("payment_intent_create requiere orderId")
	}
	allowed := map[string]bool{"connection": true, "orderId": true, "provider": true}
	for key := range args {
		if !allowed[key] {
			return fmt.Errorf("payment_intent_create no admite el argumento %q", key)
		}
	}
	if provider := strings.TrimSpace(args["provider"]); provider != "" && provider != "manual" {
		return fmt.Errorf("payment_intent_create solo admite el proveedor manual")
	}
	return nil
}

func validatePaymentSubmitArgs(args map[string]string) error {
	if err := requireFixedConnection(args, "payment_submit"); err != nil {
		return err
	}
	if strings.TrimSpace(args["paymentId"]) == "" {
		return fmt.Errorf("payment_submit requiere paymentId")
	}
	if strings.TrimSpace(args["reference"]) == "" {
		return fmt.Errorf("payment_submit requiere el número de operación declarado")
	}
	allowed := map[string]bool{
		"connection": true, "paymentId": true, "reference": true, "receiptUrl": true,
		"declaredAmount": true, "declaredAt": true, "channel": true,
		"payerName": true, "recipient": true,
	}
	for key := range args {
		if !allowed[key] {
			return fmt.Errorf("payment_submit no admite el argumento %q", key)
		}
	}
	return nil
}

func validateDataMutateArgs(args map[string]string) error {
	object := strings.TrimSpace(args["object"])
	operation := strings.TrimSpace(args["operation"])
	if object == "" || strings.Contains(object, "{") {
		return fmt.Errorf("data_mutate requiere un objeto fijo elegido por el autor")
	}
	if operation != "create" && operation != "update" && operation != "upsert" {
		return fmt.Errorf("data_mutate requiere operation create, update o upsert")
	}
	if strings.TrimSpace(args["idempotencyKey"]) == "" {
		return fmt.Errorf("data_mutate requiere idempotencyKey")
	}
	if operation == "update" && strings.TrimSpace(args["recordId"]) == "" {
		return fmt.Errorf("data_mutate update requiere recordId")
	}
	if operation == "upsert" && (strings.TrimSpace(args["matchField"]) == "" ||
		strings.Contains(args["matchField"], "{") || strings.TrimSpace(args["matchValue"]) == "") {
		return fmt.Errorf("data_mutate upsert requiere matchField fijo y matchValue")
	}
	if raw := strings.TrimSpace(args["linkCurrentContact"]); raw != "" {
		if _, err := strconv.ParseBool(raw); err != nil {
			return fmt.Errorf("data_mutate linkCurrentContact debe ser true o false")
		}
	}
	allowed := map[string]bool{
		"object": true, "operation": true, "recordId": true, "matchField": true,
		"matchValue": true, "linkCurrentContact": true, "idempotencyKey": true,
	}
	fieldCount := 0
	for key := range args {
		if allowed[key] {
			continue
		}
		if strings.HasPrefix(key, "field.") && branchNameRe.MatchString(strings.TrimPrefix(key, "field.")) {
			fieldCount++
			continue
		}
		return fmt.Errorf("data_mutate no admite el argumento %q", key)
	}
	if fieldCount == 0 {
		return fmt.Errorf("data_mutate requiere al menos un argumento field.<campo>")
	}
	return nil
}

func validateSubscriptionActivateArgs(args map[string]string) error {
	required := []string{"activationCode", "planKey", "billingCycle", "paymentRecordId", "idempotencyKey"}
	allowed := map[string]bool{"activationCode": true, "planKey": true, "billingCycle": true,
		"paymentRecordId": true, "phone": true, "idempotencyKey": true}
	for _, key := range required {
		if strings.TrimSpace(args[key]) == "" {
			return fmt.Errorf("subscription_activate requiere %s", key)
		}
	}
	for key := range args {
		if !allowed[key] {
			return fmt.Errorf("subscription_activate no admite el argumento %q", key)
		}
	}
	return nil
}

func validateCreditRechargeActivateArgs(args map[string]string) error {
	required := []string{"activationCode", "paymentRecordId", "idempotencyKey"}
	allowed := map[string]bool{"activationCode": true, "creditsAmount": true, "amount": true,
		"paymentRecordId": true, "phone": true, "notes": true, "idempotencyKey": true}
	for _, key := range required {
		if strings.TrimSpace(args[key]) == "" {
			return fmt.Errorf("credit_recharge_activate requiere %s", key)
		}
	}
	if strings.TrimSpace(args["creditsAmount"]) == "" && strings.TrimSpace(args["amount"]) == "" {
		return fmt.Errorf("credit_recharge_activate requiere creditsAmount o amount")
	}
	for key := range args {
		if !allowed[key] {
			return fmt.Errorf("credit_recharge_activate no admite el argumento %q", key)
		}
	}
	return nil
}

// validateAgentTools comprueba las herramientas que el nodo le ofrece al modelo.
//
// No hay ramas que validar aquí, a diferencia del bloque `tool`: el resultado de
// una llamada vuelve al contexto del modelo, no a una arista. Lo que sí hay que
// garantizar es que la herramienta existe, que está pensada para un agente y que
// el autor fijó la configuración que el modelo no puede elegir.
func validateAgentTools(nodeTools []NodeTool) error {
	if len(nodeTools) > 16 {
		return fmt.Errorf("demasiadas herramientas (%d): el modelo elige peor cuantas más tenga", len(nodeTools))
	}
	seen := make(map[string]struct{}, len(nodeTools))
	for _, nodeTool := range nodeTools {
		spec := tools.Get(nodeTool.Ref)
		if spec == nil {
			return fmt.Errorf("herramienta %q no implementada", nodeTool.Ref)
		}
		if !spec.ForAgent {
			return fmt.Errorf("la herramienta %q no está disponible para agentes", nodeTool.Ref)
		}
		if _, exists := seen[nodeTool.Ref]; exists {
			return fmt.Errorf("herramienta duplicada %q", nodeTool.Ref)
		}
		seen[nodeTool.Ref] = struct{}{}
		for _, key := range spec.Config {
			if key.Required && strings.TrimSpace(nodeTool.Config[key.Key]) == "" {
				return fmt.Errorf("la herramienta %q requiere %s", nodeTool.Ref, key.Label)
			}
		}
		for key := range nodeTool.Config {
			if !specHasConfig(spec, key) {
				return fmt.Errorf("la herramienta %q no admite la configuración %q", nodeTool.Ref, key)
			}
		}
		if nodeTool.Ref == "data_query" {
			object := strings.TrimSpace(nodeTool.Config["object"])
			objects := strings.TrimSpace(nodeTool.Config["objects"])
			if (object == "") == (objects == "") {
				return fmt.Errorf("la herramienta data_query requiere Objeto de datos (object) u objects, pero no ambos")
			}
			if strings.Contains(object, "{") || strings.Contains(objects, "{") {
				return fmt.Errorf("la herramienta data_query requiere objetos fijos")
			}
			if objects != "" && (strings.TrimSpace(nodeTool.Config["fields"]) != "" || strings.TrimSpace(nodeTool.Config["filterFields"]) != "") {
				return fmt.Errorf("data_query con varios objetos no admite fields ni filterFields compartidos")
			}
		}
		if nodeTool.Ref == "catalog_search" || nodeTool.Ref == "catalog_product" {
			// Ninguna configuración del catálogo interpola: el modelo redacta los
			// valores de búsqueda, nunca a qué tienda se pregunta ni con qué tope.
			for key, value := range nodeTool.Config {
				if strings.Contains(value, "{") && key != "urlTemplate" {
					return fmt.Errorf("la herramienta %s requiere un %s fijo, sin variables", nodeTool.Ref, key)
				}
			}
			if err := validateURLTemplate(nodeTool.Config["urlTemplate"]); err != nil {
				return err
			}
		}
		if nodeTool.Ref == "catalog_search" {
			if raw := strings.TrimSpace(nodeTool.Config["maxLimit"]); raw != "" {
				limit, err := strconv.Atoi(raw)
				if err != nil || limit <= 0 {
					return fmt.Errorf("catalog_search requiere un máximo de productos positivo")
				}
				if limit > maxCatalogResults {
					return fmt.Errorf("catalog_search admite como mucho %d productos por consulta", maxCatalogResults)
				}
			}
			if raw := strings.TrimSpace(nodeTool.Config["categoryId"]); raw != "" {
				if id, err := strconv.Atoi(raw); err != nil || id <= 0 {
					return fmt.Errorf("catalog_search requiere una categoría numérica")
				}
			}
		}
	}
	return nil
}

func specHasConfig(spec *tools.Spec, key string) bool {
	for _, item := range spec.Config {
		if item.Key == key {
			return true
		}
	}
	return false
}

func validateInputTypes(inputTypes []string) error {
	valid := map[string]bool{
		"text": true, "image": true, "audio": true, "document": true,
		"video": true, "sticker": true, "location": true, "contacts": true,
		"interactive": true, "order": true, "reaction": true, "unsupported": true,
	}
	seen := map[string]bool{}
	for _, inputType := range inputTypes {
		inputType = normalizedInputType(strings.TrimSpace(inputType))
		if !valid[inputType] {
			return fmt.Errorf("tipo de entrada %q inválido", inputType)
		}
		if seen[inputType] {
			return fmt.Errorf("tipo de entrada duplicado %q", inputType)
		}
		seen[inputType] = true
	}
	return nil
}

func requiredHandles(n *Node) []string {
	switch n.Kind {
	case "agent":
		return n.Outputs
	case "wait":
		if len(n.Accepts) > 1 {
			return n.Accepts
		}
	case "condition":
		return []string{"true", "false"}
	case "router":
		handles := make([]string, 0, len(n.Cases)+1)
		for _, routeCase := range n.Cases {
			handles = append(handles, routeCase.ID)
		}
		return append(handles, "default")
	case "tool":
		return []string{"ok", "error"}
	case "action":
		if n.Action == "end" {
			return nil
		}
	}
	return nil
}

func isLinearNode(n *Node) bool {
	return n.Kind == "send" || n.Kind == "wait" || n.Kind == "action" && n.Action != "end"
}

func outgoingCount(handles map[string]int) int {
	total := 0
	for _, count := range handles {
		total += count
	}
	return total
}

func pathExists(edges []Edge, from, target string, excludedEdgeIndex int) bool {
	seen := map[string]bool{}
	queue := []string{from}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current == target {
			return true
		}
		if seen[current] {
			continue
		}
		seen[current] = true
		for edgeIndex, edge := range edges {
			if edgeIndex != excludedEdgeIndex && edge.Source == current {
				queue = append(queue, edge.Target)
			}
		}
	}
	return false
}

// automaticCycleWithoutWait elimina los nodos wait y busca componentes
// fuertemente conexos en lo que queda. Un ciclo que sobrevive a ese corte puede
// avanzar indefinidamente dentro de una sola llamada a Advance; que alguna
// arista lleve role=loopback no cambia esa propiedad. Eliminar los wait, en vez
// de aceptar cualquier SCC que contenga uno, también detecta un bypass automático
// dentro de un componente que posee otra rama pausada.
func automaticCycleWithoutWait(orderedNodes []Node, edges []Edge, nodes map[string]*Node) []string {
	adjacency := make(map[string][]string, len(orderedNodes))
	for _, edge := range edges {
		source, target := nodes[edge.Source], nodes[edge.Target]
		if source == nil || target == nil || source.Kind == "wait" || target.Kind == "wait" {
			continue
		}
		adjacency[edge.Source] = append(adjacency[edge.Source], edge.Target)
	}

	nextIndex := 0
	indexes := make(map[string]int, len(orderedNodes))
	lowlinks := make(map[string]int, len(orderedNodes))
	onStack := make(map[string]bool, len(orderedNodes))
	stack := make([]string, 0, len(orderedNodes))
	var cycle []string

	var strongConnect func(string)
	strongConnect = func(id string) {
		nextIndex++
		indexes[id] = nextIndex
		lowlinks[id] = nextIndex
		stack = append(stack, id)
		onStack[id] = true

		for _, target := range adjacency[id] {
			if _, visited := indexes[target]; !visited {
				strongConnect(target)
				if lowlinks[target] < lowlinks[id] {
					lowlinks[id] = lowlinks[target]
				}
			} else if onStack[target] && indexes[target] < lowlinks[id] {
				lowlinks[id] = indexes[target]
			}
		}

		if lowlinks[id] != indexes[id] {
			return
		}
		component := make([]string, 0, 1)
		for len(stack) > 0 {
			last := len(stack) - 1
			member := stack[last]
			stack = stack[:last]
			onStack[member] = false
			component = append(component, member)
			if member == id {
				break
			}
		}

		cyclic := len(component) > 1
		if !cyclic {
			for _, target := range adjacency[id] {
				if target == id {
					cyclic = true
					break
				}
			}
		}
		if cyclic && len(cycle) == 0 {
			cycle = component
			sort.Strings(cycle)
		}
	}

	for _, node := range orderedNodes {
		if len(cycle) > 0 {
			break
		}
		if node.Kind == "wait" {
			continue
		}
		if _, visited := indexes[node.ID]; !visited {
			strongConnect(node.ID)
		}
	}
	return cycle
}
