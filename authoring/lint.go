package authoring

import (
	"fmt"
	"strings"
)

// LintCandidate reports quality risks only. It never replaces engine or
// binding errors and therefore emits warnings exclusively.
func LintCandidate(raw []byte) []Diagnostic {
	document, err := ParseDocument(raw)
	if err != nil {
		return nil
	}
	nodes, err := objectArrayField(document, "nodes")
	if err != nil {
		return nil
	}
	edges, err := objectArrayField(document, "edges")
	if err != nil {
		return nil
	}
	nodeIndex, err := indexByID(nodes, "nodo")
	if err != nil {
		return nil
	}
	outgoing := make(map[string][]map[string]any)
	for _, edge := range edges {
		source, _ := edge["source"].(string)
		outgoing[source] = append(outgoing[source], edge)
	}
	diagnostics := make([]Diagnostic, 0)
	for _, node := range nodes {
		nodeID, _ := node["id"].(string)
		kind, _ := node["kind"].(string)
		if kind == "tool" {
			toolRef, _ := node["toolRef"].(string)
			if !toolErrorRouteRecovers(nodeID, outgoing, nodeIndex) {
				diagnostics = append(diagnostics, Diagnostic{
					Severity: SeverityWarning, Source: SourceLint, Code: "unhandled_tool_error",
					NodeID: nodeID, Path: "edges.error",
					Message: fmt.Sprintf("la ruta error de %s no alcanza un mensaje, una espera, un fin o un handoff", toolRef),
				})
			}
			args := stringObject(node["args"])
			if toolRef == "data_mutate" || toolRef == "order_create" {
				key := strings.TrimSpace(args["idempotencyKey"])
				if key != "" && !strings.Contains(key, "{") {
					diagnostics = append(diagnostics, Diagnostic{
						Severity: SeverityWarning, Source: SourceLint, Code: "unstable_idempotency_key",
						NodeID: nodeID, Path: "args.idempotencyKey",
						Message: "la clave de idempotencia es un literal fijo y reutilizaría la misma operación entre mensajes",
					})
				}
			}
		}
		if kind == "agent" {
			diagnostics = append(diagnostics, lintDoubleResponses(nodeID, node, outgoing, nodeIndex)...)
		}
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

func toolErrorRouteRecovers(nodeID string, outgoing map[string][]map[string]any, nodes map[string]map[string]any) bool {
	start := ""
	for _, edge := range outgoing[nodeID] {
		if edge["sourceHandle"] == "error" {
			start, _ = edge["target"].(string)
			break
		}
	}
	if start == "" {
		return false
	}
	queue := []string{start}
	seen := map[string]bool{}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if seen[current] {
			continue
		}
		seen[current] = true
		node := nodes[current]
		kind, _ := node["kind"].(string)
		switch kind {
		case "send", "wait":
			return true
		case "action":
			action, _ := node["action"].(string)
			if action == "handoff" || action == "end" {
				return true
			}
		}
		for _, edge := range outgoing[current] {
			if target, ok := edge["target"].(string); ok {
				queue = append(queue, target)
			}
		}
	}
	return false
}

func lintDoubleResponses(nodeID string, node map[string]any, outgoing map[string][]map[string]any, nodes map[string]map[string]any) []Diagnostic {
	if silent, _ := node["silent"].(bool); silent {
		return nil
	}
	replyOn := make(map[string]bool)
	if items, ok := node["replyOn"].([]any); ok && len(items) > 0 {
		for _, item := range items {
			if branch, ok := item.(string); ok {
				replyOn[branch] = true
			}
		}
	} else if outputs, ok := node["outputs"].([]any); ok {
		for _, item := range outputs {
			if branch, ok := item.(string); ok {
				replyOn[branch] = true
			}
		}
	}
	diagnostics := make([]Diagnostic, 0)
	for _, edge := range outgoing[nodeID] {
		handle, _ := edge["sourceHandle"].(string)
		if !replyOn[handle] {
			continue
		}
		targetID, _ := edge["target"].(string)
		if target := nodes[targetID]; target != nil && target["kind"] == "send" {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: SeverityWarning, Source: SourceLint, Code: "double_response_branch",
				NodeID: nodeID, Path: "replyOn",
				Message: fmt.Sprintf("la rama %s hace responder al agente y luego ejecuta el send %s", handle, targetID),
			})
		}
	}
	return diagnostics
}
