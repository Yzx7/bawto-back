package engine

import (
	"fmt"
	"strings"
)

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
	return nil
}

func validateNode(n *Node, triggerType string) error {
	switch n.Kind {
	case "send":
		if n.Body == "" && n.TemplateName == "" {
			return fmt.Errorf("requiere texto o plantilla")
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
	case "wait":
		if triggerType != "message" {
			return fmt.Errorf("solo se puede esperar en flujos de mensajes")
		}
		if n.Expect != "any" && n.Expect != "text" && n.Expect != "image" {
			return fmt.Errorf("tipo de espera inválido")
		}
	case "tool":
		if n.ToolRef != "record_payment_receipt" {
			return fmt.Errorf("herramienta %q no implementada", n.ToolRef)
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
