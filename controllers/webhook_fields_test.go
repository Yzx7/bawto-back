package controllers

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/Yzx7/sacs-chatbots/config"
	"github.com/Yzx7/sacs-chatbots/env"
)

func loggerBuffer() (*env.Env, *bytes.Buffer) {
	var buf bytes.Buffer
	return &env.Env{
		Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Config: &config.Config{},
	}, &buf
}

// Un campo suscrito que ningún parser reclama debe dejar traza. Sin esto el
// webhook responde 200, el log dice "webhook recibido" con el tamaño en bytes y
// ahí muere: fue exactamente lo que ocultó durante semanas seis campos sin
// receptor, y solo se descubrió mirando el panel de Meta a mano.
func TestCampoDesconocidoDejaAdvertencia(t *testing.T) {
	e, buf := loggerBuffer()
	con := New(e)

	con.warnUnhandledFields([]byte(`{"entry":[{"changes":[
		{"field":"campo_que_no_existe","value":{"a":1}}]}]}`))

	out := buf.String()
	if !strings.Contains(out, "campo de webhook sin receptor") {
		t.Fatalf("no se advirtió del campo desconocido: %q", out)
	}
	if !strings.Contains(out, "campo_que_no_existe") {
		t.Fatalf("la advertencia no nombra el campo: %q", out)
	}
	if strings.Count(out, "campo de webhook sin receptor") != 1 {
		t.Fatalf("esperaba exactamente una advertencia: %q", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("la advertencia debería ser WARN: %q", out)
	}
}

// El `value` puede traer teléfono, nombre y texto del cliente. Los logs no son
// el sitio: la carga íntegra va a channel_account_events, en Postgres y bajo la
// organización dueña.
func TestAdvertenciaNoVuelcaElContenido(t *testing.T) {
	e, buf := loggerBuffer()
	con := New(e)

	con.warnUnhandledFields([]byte(`{"entry":[{"changes":[{"field":"desconocido","value":
		{"telefono":"51999888777","texto":"datos privados del cliente"}}]}]}`))

	out := buf.String()
	for _, secreto := range []string{"51999888777", "datos privados del cliente", "telefono"} {
		if strings.Contains(out, secreto) {
			t.Fatalf("el log filtró contenido del cliente (%q): %s", secreto, out)
		}
	}
}

// Los campos con receptor no deben generar ruido, o la advertencia se volvería
// inútil por costumbre.
func TestCamposConReceptorNoAdvierten(t *testing.T) {
	e, buf := loggerBuffer()
	con := New(e)

	con.warnUnhandledFields([]byte(`{"entry":[{"changes":[
		{"field":"messages","value":{}},
		{"field":"smb_message_echoes","value":{}},
		{"field":"message_template_status_update","value":{}},
		{"field":"template_category_update","value":{}},
		{"field":"account_update","value":{}},
		{"field":"phone_number_quality_update","value":{}},
		{"field":"account_alerts","value":{}},
		{"field":"account_review_update","value":{}},
		{"field":"security","value":{}},
		{"field":"phone_number_name_update","value":{}}]}]}`))

	if out := buf.String(); strings.Contains(out, "sin receptor") {
		t.Fatalf("un campo con receptor generó advertencia: %q", out)
	}
}

// Un campo desconocido no puede impedir que se procesen los conocidos que
// vengan en el mismo entry.
func TestCampoDesconocidoNoAfectaALosDemas(t *testing.T) {
	e, buf := loggerBuffer()
	con := New(e)

	con.warnUnhandledFields([]byte(`{"entry":[{"changes":[
		{"field":"messages","value":{}},
		{"field":"otro_desconocido","value":{}},
		{"field":"account_update","value":{}}]}]}`))

	out := buf.String()
	if strings.Count(out, "sin receptor") != 1 {
		t.Fatalf("esperaba una sola advertencia: %q", out)
	}
	if !strings.Contains(out, "otro_desconocido") {
		t.Fatalf("advirtió del campo equivocado: %q", out)
	}
}
