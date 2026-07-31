package controllers

import (
	"encoding/json"
	"testing"
)

func TestValidateFlowDefinition(t *testing.T) {
	valido := json.RawMessage(`{
		"id":"f1","name":"F","trigger":{"type":"message","match":"any"},
		"nodes":[{"id":"n1","kind":"send","body":"hola"},{"id":"n2","kind":"action","action":"end"}],
		"edges":[{"id":"e0","source":"trigger","target":"n1"},{"id":"e1","source":"n1","target":"n2"}]
	}`)
	if problem := validateFlowDefinition(valido, "message"); problem != "" {
		t.Fatalf("grafo válido rechazado: %s", problem)
	}
	// El trigger del grafo y el del flujo no pueden divergir: si no, un flujo
	// declarado `schedule` publicaría un grafo conversacional.
	if problem := validateFlowDefinition(valido, "schedule"); problem == "" {
		t.Fatal("se esperaba error por trigger que no coincide")
	}
	if problem := validateFlowDefinition(json.RawMessage(``), "message"); problem == "" {
		t.Fatal("un flujo vacío no puede publicarse")
	}
	// Regla de §3.6: un schedule no puede esperar respuestas.
	schedule := json.RawMessage(`{
		"id":"f2","name":"S","trigger":{"type":"schedule","cron":"0 9 * * *","viewId":"v1"},
		"nodes":[{"id":"n1","kind":"wait","expect":"any"},{"id":"n2","kind":"action","action":"end"}],
		"edges":[{"id":"e0","source":"trigger","target":"n1"},{"id":"e1","source":"n1","target":"n2"}]
	}`)
	if problem := validateFlowDefinition(schedule, "schedule"); problem == "" {
		t.Fatal("un flujo schedule con nodo wait no puede publicarse")
	}
}
