package whatsapp

import (
	"testing"
	"time"
)

func TestParseUserPreferences(t *testing.T) {
	body := []byte(`{"entry":[{"id":"WABA1","time":1754540000,"changes":[{
		"field":"user_preferences","value":{
			"messaging_product":"whatsapp",
			"metadata":{"display_phone_number":"51900","phone_number_id":"PN1"},
			"user_preferences":[
				{"wa_id":"51999888777","detail":"User requested to stop marketing messages",
				 "category":"marketing_messages","value":"stop","timestamp":"1754540100"},
				{"wa_id":"51999000111","category":"marketing_messages","value":"resume","timestamp":"1754540200"}
			]}}]}]}`)

	prefs, err := ParseUserPreferences(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(prefs) != 2 {
		t.Fatalf("esperaba 2 preferencias, got %d: %+v", len(prefs), prefs)
	}

	stop := prefs[0]
	if stop.ChannelID != "PN1" || stop.WaID != "51999888777" {
		t.Fatalf("identificadores mal extraídos: %+v", stop)
	}
	if !stop.OptedOut() || !stop.IsMarketing() {
		t.Fatalf("no se reconoció el opt-out: %+v", stop)
	}
	if stop.Detail == "" {
		t.Fatalf("se perdió el detalle: %+v", stop)
	}
	if !stop.OccurredAt.Equal(time.Unix(1754540100, 0).UTC()) {
		t.Fatalf("timestamp mal interpretado: %v", stop.OccurredAt)
	}
	if prefs[1].OptedOut() {
		t.Fatalf("un resume no puede leerse como opt-out: %+v", prefs[1])
	}
	if prefs[0].EventKey == prefs[1].EventKey {
		t.Fatal("dos usuarios distintos comparten event_key")
	}
}

// El mismo evento reentregado debe producir la misma clave; uno posterior, otra.
// Sin esto un reintento de Meta duplicaría el registro, y un cambio real se
// descartaría como si fuera un reintento.
func TestParseUserPreferencesEventKey(t *testing.T) {
	build := func(ts string) []byte {
		return []byte(`{"entry":[{"time":1754540000,"changes":[{"field":"user_preferences","value":{
			"metadata":{"phone_number_id":"PN1"},
			"user_preferences":[{"wa_id":"51999888777","category":"marketing_messages",
			 "value":"stop","timestamp":"` + ts + `"}]}}]}]}`)
	}
	a, _ := ParseUserPreferences(build("1754540100"))
	repetido, _ := ParseUserPreferences(build("1754540100"))
	despues, _ := ParseUserPreferences(build("1754540900"))

	if a[0].EventKey != repetido[0].EventKey {
		t.Fatal("el reintento produciría un registro duplicado")
	}
	if a[0].EventKey == despues[0].EventKey {
		t.Fatal("un cambio posterior se descartaría como reintento")
	}
}

// Meta manda unos timestamps como número y otros como cadena. Equivocarse aquí
// desordenaría la guarda que impide que un `resume` viejo pise un `stop` nuevo.
func TestParseUserPreferencesTimestampNumerico(t *testing.T) {
	body := []byte(`{"entry":[{"time":1754540000,"changes":[{"field":"user_preferences","value":{
		"metadata":{"phone_number_id":"PN1"},
		"user_preferences":[{"wa_id":"51999888777","category":"marketing_messages",
		 "value":"stop","timestamp":1754540100}]}}]}]}`)
	prefs, err := ParseUserPreferences(body)
	if err != nil || len(prefs) != 1 {
		t.Fatalf("parse err=%v n=%d", err, len(prefs))
	}
	if !prefs[0].OccurredAt.Equal(time.Unix(1754540100, 0).UTC()) {
		t.Fatalf("timestamp numérico mal interpretado: %v", prefs[0].OccurredAt)
	}
}

// Sin timestamp se cae al del entry, no a "ahora": usar la hora de proceso
// rompería el orden entre eventos entregados fuera de secuencia.
func TestParseUserPreferencesSinTimestamp(t *testing.T) {
	body := []byte(`{"entry":[{"time":1754540000,"changes":[{"field":"user_preferences","value":{
		"metadata":{"phone_number_id":"PN1"},
		"user_preferences":[{"wa_id":"51999888777","category":"marketing_messages","value":"stop"}]}}]}]}`)
	prefs, err := ParseUserPreferences(body)
	if err != nil || len(prefs) != 1 {
		t.Fatalf("parse err=%v n=%d", err, len(prefs))
	}
	if !prefs[0].OccurredAt.Equal(time.Unix(1754540000, 0).UTC()) {
		t.Fatalf("no se usó el time del entry: %v", prefs[0].OccurredAt)
	}
}

// Otros campos del mismo cuerpo no deben colarse aquí.
func TestParseUserPreferencesIgnoraOtrosCampos(t *testing.T) {
	body := []byte(`{"entry":[{"time":1,"changes":[
		{"field":"messages","value":{"messages":[]}},
		{"field":"account_update","value":{"event":"X"}}]}]}`)
	prefs, err := ParseUserPreferences(body)
	if err != nil || len(prefs) != 0 {
		t.Fatalf("no debía extraer nada: err=%v n=%d", err, len(prefs))
	}
}
