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

// Meta entregó tres mensajes seguidos sin `from` el 2026-08-15 (bot Lered). El
// código de entonces seguía adelante: `UpsertChat` creaba un chat con `contact`
// vacío —y lo reutilizaba en cada mensaje siguiente por el índice
// (bot_id, contact)—, `EnsureInboundContact` lo rechazaba con «teléfono
// inválido» y el mensaje se perdía. En el panel quedaba una conversación con
// nombre, sin mensajes y con la ventana de 24 h siempre cerrada.
//
// El pool es nil a propósito: si el mensaje volviera a llegar a la base, el
// primer acceso reventaría en vez de pasar la prueba.
func TestMensajeSinIdentidadNoLlegaALaBase(t *testing.T) {
	e, buf := loggerBuffer()
	con := New(e)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("un mensaje sin remitente alcanzó la base: %v", r)
		}
	}()

	con.processWhatsApp([]byte(`{"entry":[{"changes":[{"field":"messages","value":{
		"messaging_product":"whatsapp",
		"metadata":{"display_phone_number":"51999000111","phone_number_id":"1302561912931418"},
		"contacts":[{"profile":{"name":"Angelo"},"wa_id":"51987654321"}],
		"messages":[{"id":"wamid.SINREMITENTE","timestamp":"1755300000",
			"type":"text","text":{"body":"quiero contratar el plan"}}]}}]}]}`))

	out := buf.String()
	if !strings.Contains(out, "mensaje entrante sin identidad") {
		t.Fatalf("no se advirtió del mensaje sin identidad: %q", out)
	}
	// La forma del mensaje es lo que permite averiguar qué manda Meta.
	if !strings.Contains(out, "campos=id,text,timestamp,type") {
		t.Fatalf("la advertencia no nombra los campos del mensaje: %q", out)
	}
	if !strings.Contains(out, "wamid.SINREMITENTE") {
		t.Fatalf("la advertencia no identifica el mensaje: %q", out)
	}
}

// La advertencia no es excusa para volcar al log lo que escribió el cliente ni
// quién es: vale la forma del mensaje, nunca su contenido.
func TestAdvertenciaSinIdentidadNoFiltraContenido(t *testing.T) {
	e, buf := loggerBuffer()
	con := New(e)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("un mensaje sin remitente alcanzó la base: %v", r)
		}
	}()

	con.processWhatsApp([]byte(`{"entry":[{"changes":[{"field":"messages","value":{
		"metadata":{"phone_number_id":"1302561912931418"},
		"contacts":[{"profile":{"name":"Angelo"},"wa_id":"51987654321"}],
		"messages":[{"id":"wamid.X","type":"text","text":{"body":"datos privados"}}]}}]}]}`))

	out := buf.String()
	for _, secreto := range []string{"datos privados", "Angelo", "51987654321"} {
		if strings.Contains(out, secreto) {
			t.Fatalf("el log filtró contenido del cliente (%q): %s", secreto, out)
		}
	}
}
