package models

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCanonicalCopilotDocumentEsObjetoYAcotado(t *testing.T) {
	canonical, checksum, err := canonicalCopilotDocument(json.RawMessage(`{"id":"f","nodes":[],"edges":[]}`))
	if err != nil || !json.Valid(canonical) || checksum == "" {
		t.Fatalf("documento válido rechazado: canonical=%s checksum=%q err=%v", canonical, checksum, err)
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`[]`), json.RawMessage(`null`), json.RawMessage(`"texto"`), nil,
		json.RawMessage(`{"x":`),
	} {
		if _, _, err := canonicalCopilotDocument(raw); err == nil {
			t.Fatalf("raíz inválida aceptada: %s", raw)
		}
	}
	tooLarge := json.RawMessage(`{"value":"` + strings.Repeat("x", flowCopilotMaxDraftBytes) + `"}`)
	if _, _, err := canonicalCopilotDocument(tooLarge); err == nil {
		t.Fatal("documento mayor a 1 MiB aceptado")
	}
}

func TestNormalizeCopilotJSONExigeRaizYLímite(t *testing.T) {
	if got, err := normalizeCopilotJSON(nil, []byte(`[]`), '[', 10, "items"); err != nil || string(got) != "[]" {
		t.Fatalf("fallback inválido: got=%s err=%v", got, err)
	}
	if _, err := normalizeCopilotJSON(json.RawMessage(`{}`), []byte(`[]`), '[', 10, "items"); err == nil {
		t.Fatal("objeto aceptado donde se esperaba array")
	}
	if _, err := normalizeCopilotJSON(json.RawMessage(`["demasiado"]`), []byte(`[]`), '[', 5, "items"); err == nil {
		t.Fatal("payload sobre límite aceptado")
	}
}

func TestNormalizeCopilotToolTraceNoPersisteArgumentos(t *testing.T) {
	valid := json.RawMessage(`[{"step":1,"name":"list_data_objects","status":"finished","callId":"call-1"}]`)
	if _, err := normalizeCopilotToolTrace(valid); err != nil {
		t.Fatalf("trace saneado rechazado: %v", err)
	}
	withArguments := json.RawMessage(`[{"step":1,"name":"tool","status":"finished","arguments":{"secret":"x"}}]`)
	if _, err := normalizeCopilotToolTrace(withArguments); err == nil {
		t.Fatal("toolTrace permitió persistir argumentos")
	}
}
