package whatsapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnalyzeTemplateComponents(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"BODY","text":"Hola {{1}}, recibo {{2}}, importe {{3}}, vence {{4}}."}
	]`)
	schema, count, unsupported := AnalyzeTemplateComponents(raw, "POSITIONAL")
	if unsupported || count != 4 || len(schema) != 4 {
		t.Fatalf("análisis positional inesperado: count=%d unsupported=%v schema=%+v", count, unsupported, schema)
	}

	_, _, unsupported = AnalyzeTemplateComponents(
		json.RawMessage(`[{"type":"HEADER","format":"IMAGE"},{"type":"BODY","text":"Hola {{1}}"}]`),
		"POSITIONAL",
	)
	if !unsupported {
		t.Fatal("un header multimedia debe marcarse como no soportado")
	}

	_, _, unsupported = AnalyzeTemplateComponents(
		json.RawMessage(`[{"type":"BODY","text":"Hola {{cliente}}"}]`),
		"NAMED",
	)
	if !unsupported {
		t.Fatal("los parámetros con nombre aún no son compatibles con SendTemplate")
	}
}

func TestListTemplatesPaginaYAutoriza(t *testing.T) {
	requests := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer token-test" {
			t.Errorf("Authorization inesperado: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			_, _ = w.Write([]byte(`{"data":[{"id":"1","name":"uno","language":"es","status":"APPROVED","category":"UTILITY","components":[]}],"paging":{"next":"` + server.URL + `/next"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"2","name":"dos","language":"es","status":"PENDING","category":"UTILITY","components":[]}]}`))
	}))
	defer server.Close()

	got, err := ListTemplates(context.Background(), SendConfig{
		APIBase: server.URL, Version: "v25.0", Token: "token-test", HTTP: server.Client(),
	}, "waba")
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if requests != 2 || len(got) != 2 || got[1].Name != "dos" {
		t.Fatalf("paginación incompleta: requests=%d templates=%+v", requests, got)
	}
}

func TestParseTemplateEventsOficiales(t *testing.T) {
	body := []byte(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"waba-1","time":1746082800,"changes":[
			{"field":"message_template_status_update","value":{
				"event":"APPROVED","message_template_id":1689556908129832,
				"message_template_name":"recordatorio","message_template_language":"es",
				"reason":"NONE","message_template_category":"UTILITY"}},
			{"field":"message_template_quality_update","value":{
				"previous_quality_score":"GREEN","new_quality_score":"YELLOW",
				"message_template_id":1689556908129832,"message_template_name":"recordatorio",
				"message_template_language":"es"}},
			{"field":"template_category_update","value":{
				"message_template_id":1689556908129832,"message_template_name":"recordatorio",
				"message_template_language":"es","new_category":"UTILITY",
				"correct_category":"MARKETING","category_update_timestamp":1746169200}}
		]}]}`)
	events, err := ParseTemplateEvents(body)
	if err != nil {
		t.Fatalf("ParseTemplateEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("se esperaban 3 eventos, got %d", len(events))
	}
	if events[0].Status != "APPROVED" || events[0].Category != "UTILITY" {
		t.Fatalf("status inesperado: %+v", events[0])
	}
	if events[1].QualityScore != "YELLOW" {
		t.Fatalf("quality inesperada: %+v", events[1])
	}
	if events[2].Category != "UTILITY" || events[2].PendingCategory != "MARKETING" ||
		events[2].CategoryChangeAt == nil {
		t.Fatalf("recategorización inesperada: %+v", events[2])
	}
	again, _ := ParseTemplateEvents(body)
	if again[2].EventKey != events[2].EventKey {
		t.Fatal("el mismo webhook debe producir la misma clave idempotente")
	}
}
