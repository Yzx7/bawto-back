package engine

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Yzx7/sacs-chatbots/engine/tools"
)

var branchNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

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
	if outgoingCount(out["trigger"]) != 1 {
		return fmt.Errorf("el trigger debe tener exactamente una salida")
	}
	for _, n := range flow.Nodes {
		if isLinearNode(&n) && outgoingCount(out[n.ID]) != 1 {
			return fmt.Errorf("el nodo %s debe tener exactamente una salida", n.ID)
		}
		required := requiredHandles(&n)
		for _, handle := range required {
			if out[n.ID][handle] != 1 {
				return fmt.Errorf("el nodo %s requiere una salida %q", n.ID, handle)
			}
		}
		if n.Kind == "agent" {
			allowed := make(map[string]struct{}, len(n.Outputs))
			for _, handle := range n.Outputs {
				allowed[handle] = struct{}{}
			}
			for handle := range out[n.ID] {
				if _, exists := allowed[handle]; !exists {
					return fmt.Errorf("el nodo %s tiene una conexión para la rama no declarada %q", n.ID, handle)
				}
			}
		}
	}

	seen := map[string]bool{}
	queue := []string{flow.next("trigger", "")}
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
		if err := validateAgentTools(n.Tools); err != nil {
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
		if n.Expect != "any" && n.Expect != "text" && n.Expect != "image" {
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
	case "condition":
		if strings.TrimSpace(n.Expression) == "" {
			return fmt.Errorf("requiere una expresión")
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

func requiredHandles(n *Node) []string {
	switch n.Kind {
	case "agent":
		return n.Outputs
	case "condition":
		return []string{"true", "false"}
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
