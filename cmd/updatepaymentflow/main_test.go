package main

import (
	"strings"
	"testing"
)

func TestUpdateDocumentAddsIdempotentPaymentRoute(t *testing.T) {
	document := map[string]any{
		"nodes": []any{
			map[string]any{"id": "n_bawto_specialist", "instruction": "menciona el importe y pide una captura completa con fecha, importe y operación."},
			map[string]any{"id": "n_services_specialist", "instruction": "cobra y pide la captura completa para registrar evidencia sin modificar accesos."},
			map[string]any{"id": "n_payment_specialist", "instruction": "cobra y pide una captura completa y nítida con fecha, importe y operación."},
			map[string]any{"id": "n_payment_wait", "pos": map[string]any{"x": 0.0, "y": 0.0}},
		},
		"edges": []any{
			map[string]any{"id": "e_bawto_charge", "source": "n_bawto_specialist", "target": "n_payment_wait", "sourceHandle": "cobrar"},
			map[string]any{"id": "e_services_charge", "source": "n_services_specialist", "target": "n_payment_wait", "sourceHandle": "cobrar"},
			map[string]any{"id": "e_payment_charge", "source": "n_payment_specialist", "target": "n_payment_wait", "sourceHandle": "cobrar"},
		},
	}

	updateDocument(document)
	updateDocument(document)

	nodes := objectSlice(document, "nodes")
	for _, id := range []string{"n_read_payment_method", "n_payment_method_router", "n_send_payment_instructions", "n_payment_method_unavailable", "n_payment_method_handoff"} {
		if countNodes(nodes, id) != 1 {
			t.Fatalf("%s debe existir una sola vez", id)
		}
	}
	for _, id := range []string{"n_bawto_specialist", "n_services_specialist", "n_payment_specialist"} {
		instruction := textValue(node(nodes, id)["instruction"])
		if !strings.Contains(instruction, "instrucciones exactas de pago") || strings.Contains(instruction, "pide una captura") {
			t.Fatalf("instrucción no corregida para %s: %q", id, instruction)
		}
	}
	for _, edge := range objectSlice(document, "edges") {
		if strings.HasSuffix(textValue(edge["id"]), "_charge") && textValue(edge["target"]) != "n_read_payment_method" {
			t.Fatalf("la rama cobrar todavía salta a %s", edge["target"])
		}
	}
}

func countNodes(nodes []map[string]any, id string) int {
	count := 0
	for _, item := range nodes {
		if textValue(item["id"]) == id {
			count++
		}
	}
	return count
}
