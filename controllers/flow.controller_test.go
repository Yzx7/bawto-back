package controllers

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
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

func TestParseDraftUpdateBodyEsFailClosed(t *testing.T) {
	draft := `{"id":"f1","name":"F","trigger":{"type":"message"},"nodes":[],"edges":[]}`
	valid := []byte(`{"draft":` + draft + `,"expectedChecksum":"abc"}`)
	got, expected, problem := parseDraftUpdateBody(valid)
	if problem != "" || expected != "abc" || !json.Valid(got) {
		t.Fatalf("envelope válido rechazado: expected=%q problem=%q draft=%s", expected, problem, got)
	}

	invalid := [][]byte{
		[]byte(draft),
		[]byte(`{"draft":` + draft + `}`),
		[]byte(`{"draft":` + draft + `,"expectedChecksum":"abc","extra":true}`),
		[]byte(`{"draft":{},"expectedChecksum":"abc"}`),
		[]byte(`{"draft":{"id":"f1","name":"F","trigger":{},"nodes":null,"edges":[]},"expectedChecksum":"abc"}`),
	}
	for _, body := range invalid {
		if _, _, problem := parseDraftUpdateBody(body); problem == "" {
			t.Fatalf("body inválido aceptado: %s", body)
		}
	}
}

func TestParseExpectedDraftChecksumBody(t *testing.T) {
	if got, problem := parseExpectedDraftChecksumBody([]byte(`{"expectedDraftChecksum":"abc"}`)); problem != "" || got != "abc" {
		t.Fatalf("checksum válido rechazado: got=%q problem=%q", got, problem)
	}
	for _, body := range [][]byte{
		{},
		[]byte(`{}`),
		[]byte(`{"expectedChecksum":"abc"}`),
		[]byte(`{"expectedDraftChecksum":""}`),
		[]byte(`{"expectedDraftChecksum":"abc","extra":true}`),
	} {
		if _, problem := parseExpectedDraftChecksumBody(body); problem == "" {
			t.Fatalf("body inválido aceptado: %s", body)
		}
	}
}

func TestFailFlowSerializaConflictoCASDentroDeGenRes(t *testing.T) {
	app := fiber.New()
	con := &Controller{}
	app.Get("/", func(c *fiber.Ctx) error {
		return con.failFlow(c, "test", "bot", &models.DraftConflictError{
			Code: "draft_conflict", ExpectedChecksum: "old", CurrentChecksum: "new",
			CurrentDraft: json.RawMessage(`{"id":"f"}`),
		}, "fallback")
	})
	response, err := app.Test(httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != fiber.StatusConflict {
		t.Fatalf("status=%d", response.StatusCode)
	}
	var envelope types.GenRes
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode GenRes: %v", err)
	}
	data, ok := envelope.Data.(map[string]any)
	if !ok || envelope.Ok || data["code"] != "draft_conflict" || data["currentChecksum"] != "new" {
		t.Fatalf("GenRes de conflicto incompleto: %+v", envelope)
	}
}
