package whatsapp

import (
	"encoding/json"
	"testing"
)

func TestParseAccountEventsCamposYSeveridad(t *testing.T) {
	body := []byte(`{"entry":[{"id":"WABA1","time":1754500000,"changes":[
		{"field":"phone_number_quality_update","value":{"phone_number_id":"PN1","event":"FLAGGED","current_limit":"TIER_1K"}},
		{"field":"account_review_update","value":{"decision":"APPROVED"}},
		{"field":"phone_number_name_update","value":{"phone_number_id":"PN1","decision":"REJECTED"}},
		{"field":"business_capability_update","value":{"max_phone_numbers_per_business":"10"}},
		{"field":"messages","value":{"messages":[]}}
	]}]}`)

	events, err := ParseAccountEvents(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// `messages` no es un campo de cuenta: no debe salir de aquí.
	if len(events) != 4 {
		t.Fatalf("esperaba 4 eventos de cuenta, got %d: %+v", len(events), events)
	}

	byField := map[string]AccountEvent{}
	for _, e := range events {
		byField[e.Field] = e
		if e.WabaID != "WABA1" {
			t.Fatalf("waba incorrecto en %s: %q", e.Field, e.WabaID)
		}
		if e.EventKey == "" {
			t.Fatalf("event_key vacío en %s", e.Field)
		}
		if e.OccurredAt.IsZero() {
			t.Fatalf("occurred_at vacío en %s", e.Field)
		}
	}

	quality := byField["phone_number_quality_update"]
	if quality.PhoneNumberID != "PN1" || quality.Event != "FLAGGED" || quality.MessagingLimit != "TIER_1K" {
		t.Fatalf("calidad mal extraída: %+v", quality)
	}
	if byField["account_review_update"].Decision != "APPROVED" {
		t.Fatalf("decisión de revisión mal extraída: %+v", byField["account_review_update"])
	}
	// Ante la duda, warning: no se inventa que REJECTED sea crítico.
	if got := byField["phone_number_name_update"].Severity; got != SeverityWarning {
		t.Fatalf("severidad por defecto: %q, esperaba warning", got)
	}
	// El único rutinario: informa de límites, no de una degradación.
	if got := byField["business_capability_update"].Severity; got != SeverityInfo {
		t.Fatalf("capability debería ser info, got %q", got)
	}
}

// La presencia de estructuras de baneo/infracción/restricción es estructural, no
// un enum que haya que adivinar: eleva a crítico sin mirar su contenido.
func TestParseAccountEventsCriticoPorEstructura(t *testing.T) {
	for _, campo := range []string{"ban_info", "violation_info", "restriction_info"} {
		body := []byte(`{"entry":[{"id":"WABA1","time":1754500000,"changes":[
			{"field":"account_update","value":{"event":"LO_QUE_SEA","` + campo + `":{"x":1}}}]}]}`)
		events, err := ParseAccountEvents(body)
		if err != nil || len(events) != 1 {
			t.Fatalf("%s: parse err=%v n=%d", campo, err, len(events))
		}
		if events[0].Severity != SeverityCritical {
			t.Fatalf("%s debería ser crítico, got %q", campo, events[0].Severity)
		}
	}

	// Un null explícito no es presencia.
	body := []byte(`{"entry":[{"id":"WABA1","time":1754500000,"changes":[
		{"field":"account_update","value":{"event":"X","ban_info":null}}]}]}`)
	events, _ := ParseAccountEvents(body)
	if len(events) != 1 || events[0].Severity != SeverityWarning {
		t.Fatalf("ban_info null no debería ser crítico: %+v", events)
	}
}

// El payload se conserva íntegro: es lo que permite revisar mañana una
// interpretación equivocada de hoy.
func TestParseAccountEventsConservaPayload(t *testing.T) {
	body := []byte(`{"entry":[{"id":"WABA1","time":1754500000,"changes":[
		{"field":"security","value":{"phone_number_id":"PN9","event":"X","campo_futuro":{"a":[1,2]}}}]}]}`)
	events, err := ParseAccountEvents(body)
	if err != nil || len(events) != 1 {
		t.Fatalf("parse err=%v n=%d", err, len(events))
	}
	var back map[string]any
	if err := json.Unmarshal(events[0].Payload, &back); err != nil {
		t.Fatalf("payload no es JSON válido: %v", err)
	}
	if _, ok := back["campo_futuro"]; !ok {
		t.Fatalf("se perdió una clave desconocida del payload: %v", back)
	}
}

// Dos entregas del mismo evento comparten event_key (Meta reintenta); dos
// eventos distintos no.
func TestParseAccountEventsEventKeyEstable(t *testing.T) {
	body := []byte(`{"entry":[{"id":"WABA1","time":1754500000,"changes":[
		{"field":"account_update","value":{"event":"A"}}]}]}`)
	otro := []byte(`{"entry":[{"id":"WABA1","time":1754500000,"changes":[
		{"field":"account_update","value":{"event":"B"}}]}]}`)

	a, _ := ParseAccountEvents(body)
	repetido, _ := ParseAccountEvents(body)
	b, _ := ParseAccountEvents(otro)
	if a[0].EventKey != repetido[0].EventKey {
		t.Fatal("el mismo evento produjo event_keys distintas: el reintento se duplicaría")
	}
	if a[0].EventKey == b[0].EventKey {
		t.Fatal("eventos distintos comparten event_key: el segundo se descartaría")
	}
}

// Un elemento ilegible no debe invalidar el lote entero.
func TestParseAccountEventsToleraBasura(t *testing.T) {
	body := []byte(`{"entry":[{"id":"WABA1","time":1754500000,"changes":[
		{"field":"account_update","value":"esto no es un objeto"},
		{"field":"security","value":{"event":"OK"}}]}]}`)
	events, err := ParseAccountEvents(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].Field != "security" {
		t.Fatalf("esperaba conservar solo el evento legible: %+v", events)
	}
}
