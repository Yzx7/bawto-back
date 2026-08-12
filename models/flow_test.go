package models

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Yzx7/sacs-chatbots/engine"
)

func TestValidFlowKey(t *testing.T) {
	ok := []string{"atencion", "recordatorio-d3", "flow_waa_isp", "a"}
	bad := []string{"", "Atencion", "3dias", "-x", "con espacio", "acentuación"}
	for _, k := range ok {
		if !ValidFlowKey(k) {
			t.Fatalf("%q debería ser válida", k)
		}
	}
	for _, k := range bad {
		if ValidFlowKey(k) {
			t.Fatalf("%q no debería ser válida", k)
		}
	}
}

// La key del backfill se deriva del propio grafo: es lo que hace que
// re-ejecutarlo no duplique flujos.
func TestFlowKeyFromDefinition(t *testing.T) {
	cases := []struct{ flowID, fallback, want string }{
		{"flow_waa_isp", "WAA", "flow_waa_isp"},
		{"", "WAA — Atención integral ISP", "waa-atencin-integral-isp"},
		{"", "", "principal"},
		{"123", "", "principal"},
		{"Recordatorio D-3", "", "recordatorio-d-3"},
	}
	for _, c := range cases {
		if got := FlowKeyFromDefinition(c.flowID, c.fallback); got != c.want {
			t.Fatalf("FlowKeyFromDefinition(%q,%q) = %q, se esperaba %q", c.flowID, c.fallback, got, c.want)
		}
	}
	// Sea cual sea la entrada, el resultado tiene que pasar el CHECK de la tabla.
	for _, in := range []string{"", "###", "ÑOÑO", "x" + string(make([]byte, 200))} {
		if k := FlowKeyFromDefinition(in, ""); !ValidFlowKey(k) {
			t.Fatalf("key derivada inválida para %q: %q", in, k)
		}
	}
}

func TestFlowJSONExponeSoloDraftSnapshot(t *testing.T) {
	actor := "editor-1"
	updatedAt := time.Date(2026, 8, 11, 20, 30, 0, 0, time.UTC)
	draft := json.RawMessage(`{"name":"F","id":"f1","trigger":{"type":"message"},"nodes":[],"edges":[]}`)
	capability := FlowCopilotCapability{Enabled: false, Reason: "no disponible", ProviderOperational: false}
	flow := Flow{
		ID: "flow-1", BotID: "bot-1", Draft: draft, UpdatedAt: updatedAt, UpdatedBy: &actor,
		CopilotCapability: &capability,
	}

	raw, err := json.Marshal(flow)
	if err != nil {
		t.Fatalf("Marshal Flow: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("Unmarshal Flow JSON: %v", err)
	}
	if _, exists := doc["draft"]; exists {
		t.Fatalf("Flow filtró el contrato viejo draft: %s", raw)
	}
	var snapshot DraftSnapshot
	if err := json.Unmarshal(doc["draftSnapshot"], &snapshot); err != nil {
		t.Fatalf("draftSnapshot ausente o inválido: %v (%s)", err, raw)
	}
	_, expectedChecksum, _ := engine.CanonicalChecksum(draft)
	if snapshot.Checksum != expectedChecksum || snapshot.UpdatedBy == nil || *snapshot.UpdatedBy != actor || !snapshot.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("snapshot inesperado: %+v", snapshot)
	}
	var gotCapability FlowCopilotCapability
	if err := json.Unmarshal(doc["copilotCapability"], &gotCapability); err != nil || gotCapability.Enabled || gotCapability.Reason == "" {
		t.Fatalf("copilotCapability ausente o inválida: %+v err=%v (%s)", gotCapability, err, raw)
	}
}
