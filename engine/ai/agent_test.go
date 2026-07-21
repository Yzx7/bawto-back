package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentRunMock(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"MiniMax-M2","content":[{"type":"text","text":"Gracias, tu pago fue confirmado.\nDECISION: valido"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":8}}`))
	}))
	defer srv.Close()

	a := New("sk-test", srv.URL, "MiniMax-M2")
	reply, branch, err := a.Run(context.Background(), "Valida el comprobante",
		map[string]string{"input": "aquí está mi pago"}, []string{"valido", "invalido"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if branch != "valido" {
		t.Fatalf("branch esperado valido, got %q", branch)
	}
	if !strings.Contains(reply, "confirmado") || strings.Contains(reply, "DECISION") {
		t.Fatalf("reply mal separado: %q", reply)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("path esperado /v1/messages, got %q", gotPath)
	}
	if gotBody["model"] != "MiniMax-M2" {
		t.Fatalf("model esperado MiniMax-M2, got %v", gotBody["model"])
	}
}

func TestParseDecision(t *testing.T) {
	reply, branch := parseDecision("Hola.\nDECISION: invalido", []string{"valido", "invalido"})
	if reply != "Hola." || branch != "invalido" {
		t.Fatalf("con decisión: reply=%q branch=%q", reply, branch)
	}
	reply2, branch2 := parseDecision("solo texto", []string{"a", "b"})
	if reply2 != "solo texto" || branch2 != "" {
		t.Fatalf("sin decisión: reply=%q branch=%q", reply2, branch2)
	}
}
