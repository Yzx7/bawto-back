package controllers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yzx7/sacs-chatbots/config"
	"github.com/Yzx7/sacs-chatbots/engine"
	"github.com/Yzx7/sacs-chatbots/env"
	"github.com/Yzx7/sacs-chatbots/helpers"
	"github.com/Yzx7/sacs-chatbots/models"
)

// Simula el pipeline completo de WhatsApp sin Meta: mock de la Graph API + BD real.
func TestWhatsAppPipelineIntegration(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL no seteada; se omite el test de integración")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	// Mock de la Cloud API (captura el Send del eco).
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.out"}]}`))
	}))
	defer srv.Close()

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cph, _ := helpers.NewCipher(hex.EncodeToString(key))

	con := New(&env.Env{
		Postgres: pool,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Cipher:   cph,
		Config: &config.Config{
			WhatsAppAppSecret:  "appsecret",
			WhatsAppAPIBase:    srv.URL,
			WhatsAppAPIVersion: "v21.0",
		},
	})

	// Datos: usuario + org + bot conectado al canal PNID.
	uid := randHex("waitu_")
	if _, err := pool.Exec(ctx, `INSERT INTO "user"(id,name,email,"emailVerified") VALUES ($1,$2,$3,false)`,
		uid, "WA Owner", uid+"@test.local"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, uid)

	org, err := models.CreateOrganization(ctx, pool, uid, "WA Org", nil, nil)
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	defer models.DeleteOrganization(ctx, pool, org.ID)

	bot, err := models.CreateBot(ctx, pool, org.ID, "WA Bot", "wsp")
	if err != nil {
		t.Fatalf("CreateBot: %v", err)
	}
	enc, _ := cph.Encrypt("TESTTOKEN")
	if err := models.UpdateBotChannel(ctx, pool, bot.ID, "wsp", "PNID", "51900", enc); err != nil {
		t.Fatalf("UpdateBotChannel: %v", err)
	}

	payload := []byte(`{"entry":[{"changes":[{"value":{"metadata":{"phone_number_id":"PNID"},"contacts":[{"profile":{"name":"Ana"}}],"messages":[{"from":"51999888777","id":"wamid.itest1","type":"text","text":{"body":"hola"}}]}}]}]}`)

	// Procesar dos veces (idempotencia por wa_id).
	con.processWhatsApp(payload)
	con.processWhatsApp(payload)

	// El eco debió enviarse a la Cloud API con el texto correcto.
	if gotBody == nil {
		t.Fatal("la Cloud API (mock) no recibió el eco")
	}
	if gotBody["to"] != "51999888777" {
		t.Fatalf("destinatario incorrecto: %v", gotBody["to"])
	}
	txt, _ := gotBody["text"].(map[string]any)
	if txt["body"] != "Recibí: hola" {
		t.Fatalf("cuerpo del eco incorrecto: %v", txt)
	}

	// El mensaje entrante se guardó una sola vez (idempotencia).
	chatID, err := models.UpsertChat(ctx, pool, bot.ID, "51999888777", "")
	if err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	var inbound int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE chat_id = $1::uuid AND from_me = false`, chatID).Scan(&inbound); err != nil {
		t.Fatalf("count: %v", err)
	}
	if inbound != 1 {
		t.Fatalf("esperaba 1 mensaje entrante (idempotente), got %d", inbound)
	}
}

func randHex(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// Ejecuta un flujo real (send → wait → send) sobre el pipeline del webhook.
func TestWhatsAppFlowEngineIntegration(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL no seteada")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var mu sync.Mutex
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		if txt, ok := body["text"].(map[string]any); ok {
			mu.Lock()
			sent = append(sent, txt["body"].(string))
			mu.Unlock()
		}
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.out"}]}`))
	}))
	defer srv.Close()

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cph, _ := helpers.NewCipher(hex.EncodeToString(key))
	con := New(&env.Env{
		Postgres: pool,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Cipher:   cph,
		Config:   &config.Config{WhatsAppAPIBase: srv.URL, WhatsAppAPIVersion: "v21.0"},
	})

	uid := randHex("feitu_")
	_, _ = pool.Exec(ctx, `INSERT INTO "user"(id,name,email,"emailVerified") VALUES ($1,$2,$3,false)`, uid, "F", uid+"@test.local")
	defer pool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, uid)
	org, _ := models.CreateOrganization(ctx, pool, uid, "FE Org", nil, nil)
	defer models.DeleteOrganization(ctx, pool, org.ID)
	bot, _ := models.CreateBot(ctx, pool, org.ID, "FE Bot", "wsp")
	enc, _ := cph.Encrypt("TKN")
	_ = models.UpdateBotChannel(ctx, pool, bot.ID, "wsp", "PNIDF", "", enc)

	// El webhook ejecuta la versión publicada del flujo `message`, así que el
	// montaje de la prueba pasa por el mismo camino que el editor.
	flowJSON := json.RawMessage(`{"id":"f","name":"F","trigger":{"type":"message","match":"any"},"nodes":[{"id":"s1","kind":"send","body":"Hola {input}"},{"id":"w1","kind":"wait"},{"id":"s2","kind":"send","body":"fin"}],"edges":[{"id":"e1","source":"trigger","target":"s1"},{"id":"e2","source":"s1","target":"w1"},{"id":"e3","source":"w1","target":"s2"}]}`)
	flow, err := models.CreateFlow(ctx, pool, bot.ID, models.NewFlow{
		Key: "flow_fe", Name: "F", TriggerType: "message", IsFallback: true, UserID: uid,
	})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	canonical, checksum, err := engine.CanonicalChecksum(flowJSON)
	if err != nil {
		t.Fatalf("CanonicalChecksum: %v", err)
	}
	if _, err := models.PublishFlow(ctx, pool, bot.ID, flow.ID, canonical, checksum, uid); err != nil {
		t.Fatalf("PublishFlow: %v", err)
	}

	inbound := func(waID, text string) []byte {
		return []byte(`{"entry":[{"changes":[{"value":{"metadata":{"phone_number_id":"PNIDF"},"messages":[{"from":"51999888777","id":"` + waID + `","type":"text","text":{"body":"` + text + `"}}]}}]}]}`)
	}

	con.processWhatsApp(inbound("wamid.f1", "hola"))
	con.processWhatsApp(inbound("wamid.f2", "algo"))

	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 || sent[0] != "Hola hola" || sent[1] != "fin" {
		t.Fatalf("secuencia del flujo incorrecta: %v", sent)
	}
}

// Con varios flujos `message` publicados el webhook tiene que elegir uno, y esa
// elección tiene tres reglas que solo se ven juntas: gana el que reconoce el
// mensaje, una conversación a medias se queda en el suyo aunque el turno
// siguiente case con otro, y si nadie reconoce atiende el fallback.
//
//	DATABASE_URL=... go test ./controllers -run VariosFlujos -v
func TestWebhookVariosFlujosMessageEligeYReanuda(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL no seteada")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var mu sync.Mutex
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		if txt, ok := body["text"].(map[string]any); ok {
			mu.Lock()
			sent = append(sent, txt["body"].(string))
			mu.Unlock()
		}
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.out"}]}`))
	}))
	defer srv.Close()

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cph, _ := helpers.NewCipher(hex.EncodeToString(key))
	con := New(&env.Env{
		Postgres: pool,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Cipher:   cph,
		Config:   &config.Config{WhatsAppAPIBase: srv.URL, WhatsAppAPIVersion: "v21.0"},
	})

	uid := randHex("disp_")
	_, _ = pool.Exec(ctx, `INSERT INTO "user"(id,name,email,"emailVerified") VALUES ($1,$2,$3,false)`, uid, "D", uid+"@test.local")
	defer pool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, uid)
	org, _ := models.CreateOrganization(ctx, pool, uid, "Disp Org", nil, nil)
	defer models.DeleteOrganization(ctx, pool, org.ID)
	bot, _ := models.CreateBot(ctx, pool, org.ID, "Disp Bot", "wsp")
	enc, _ := cph.Encrypt("TKN")
	_ = models.UpdateBotChannel(ctx, pool, bot.ID, "wsp", "PNIDD", "", enc)

	publicar := func(flowKey, name string, priority int, fallback bool, graph string) {
		t.Helper()
		flow, err := models.CreateFlow(ctx, pool, bot.ID, models.NewFlow{
			Key: flowKey, Name: name, TriggerType: "message",
			Priority: priority, IsFallback: fallback, UserID: uid,
		})
		if err != nil {
			t.Fatalf("CreateFlow %s: %v", flowKey, err)
		}
		canonical, checksum, err := engine.CanonicalChecksum(json.RawMessage(graph))
		if err != nil {
			t.Fatalf("CanonicalChecksum %s: %v", flowKey, err)
		}
		if _, err := models.PublishFlow(ctx, pool, bot.ID, flow.ID, canonical, checksum, uid); err != nil {
			t.Fatalf("PublishFlow %s: %v", flowKey, err)
		}
	}

	// El de cobranza reconoce "pago" y deja la conversación esperando; el general
	// atiende cualquier cosa y es el fallback.
	publicar("cobranza", "Cobranza", 10, false, `{"id":"fc","name":"Cobranza",
		"trigger":{"type":"message","match":"keyword","keywords":["pago"]},
		"nodes":[{"id":"c1","kind":"send","body":"COBRANZA"},{"id":"cw","kind":"wait","expect":"any"},
		         {"id":"c2","kind":"send","body":"COBRANZA-FIN"},{"id":"cend","kind":"action","action":"end"}],
		"edges":[{"id":"ce0","source":"trigger","target":"c1"},{"id":"ce1","source":"c1","target":"cw"},
		         {"id":"ce2","source":"cw","target":"c2"},{"id":"ce3","source":"c2","target":"cend"}]}`)
	publicar("general", "General", 50, true, `{"id":"fg","name":"General",
		"trigger":{"type":"message","match":"any"},
		"nodes":[{"id":"g1","kind":"send","body":"GENERAL"},{"id":"gend","kind":"action","action":"end"}],
		"edges":[{"id":"ge0","source":"trigger","target":"g1"},{"id":"ge1","source":"g1","target":"gend"}]}`)

	inbound := func(waID, text string) []byte {
		return []byte(`{"entry":[{"changes":[{"value":{"metadata":{"phone_number_id":"PNIDD"},"messages":[{"from":"51999111222","id":"` + waID + `","type":"text","text":{"body":"` + text + `"}}]}}]}]}`)
	}

	con.processWhatsApp(inbound("wamid.d1", "quiero hacer un pago"))
	// "hola" casaría con el trigger del general, pero la conversación está a
	// medias en cobranza: reanudar allí es la regla que se está probando.
	con.processWhatsApp(inbound("wamid.d2", "hola"))
	// Ya terminó: ahora sí atiende el fallback.
	con.processWhatsApp(inbound("wamid.d3", "hola"))

	mu.Lock()
	defer mu.Unlock()
	want := []string{"COBRANZA", "COBRANZA-FIN", "GENERAL"}
	if len(sent) != len(want) {
		t.Fatalf("mensajes inesperados: %v (esperado %v)", sent, want)
	}
	for i := range want {
		if sent[i] != want[i] {
			t.Fatalf("mensaje %d = %q, esperado %q (secuencia %v)", i+1, sent[i], want[i], sent)
		}
	}
}
