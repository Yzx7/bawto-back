package engine

import (
	"fmt"
	"regexp"
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
		if edge.Role == "loopback" &&
			(nodes[edge.Source] == nil || nodes[edge.Source].Kind != "wait") &&
			(nodes[edge.Target] == nil || nodes[edge.Target].Kind != "wait") &&
			pathExistsWithoutWait(flow.Edges, nodes, edge.Target, edge.Source, edgeIndex) {
			return fmt.Errorf("la arista %s forma un ciclo sin un nodo wait", edge.ID)
		}
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
		for _, output := range n.Outputs {
			if !branchNameRe.MatchString(output) {
				return fmt.Errorf("rama %q inválida: usa 1–64 letras ASCII, números, guion o guion bajo", output)
			}
			key := strings.ToLower(output)
			if _, exists := seenOutputs[key]; exists {
				return fmt.Errorf("rama duplicada %q", output)
			}
			seenOutputs[key] = struct{}{}
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
		if n.ToolRef == "data_mutate" {
			if err := validateDataMutateArgs(n.Args); err != nil {
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

func pathExistsWithoutWait(edges []Edge, nodes map[string]*Node, from, target string, excludedEdgeIndex int) bool {
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
		if node := nodes[current]; node != nil && node.Kind == "wait" {
			continue
		}
		for edgeIndex, edge := range edges {
			if edgeIndex != excludedEdgeIndex && edge.Source == current {
				queue = append(queue, edge.Target)
			}
		}
	}
	return false
}
