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

	payload := []byte(`{"entry":[{"changes":[{"value":{"metadata":{"phone_number_id":"PNID"},"contacts":[{"profile":{"name":"Ana"}}],"messages":[{"from":"51999","id":"wamid.itest1","type":"text","text":{"body":"hola"}}]}}]}]}`)

	// Procesar dos veces (idempotencia por wa_id).
	con.processWhatsApp(payload)
	con.processWhatsApp(payload)

	// El eco debió enviarse a la Cloud API con el texto correcto.
	if gotBody == nil {
		t.Fatal("la Cloud API (mock) no recibió el eco")
	}
	if gotBody["to"] != "51999" {
		t.Fatalf("destinatario incorrecto: %v", gotBody["to"])
	}
	txt, _ := gotBody["text"].(map[string]any)
	if txt["body"] != "Recibí: hola" {
		t.Fatalf("cuerpo del eco incorrecto: %v", txt)
	}

	// El mensaje entrante se guardó una sola vez (idempotencia).
	chatID, err := models.UpsertChat(ctx, pool, bot.ID, "51999", "")
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

	flowJSON := `{"id":"f","name":"F","trigger":{"type":"message","match":"any"},"nodes":[{"id":"s1","kind":"send","body":"Hola {input}"},{"id":"w1","kind":"wait"},{"id":"s2","kind":"send","body":"fin"}],"edges":[{"id":"e1","source":"trigger","target":"s1"},{"id":"e2","source":"s1","target":"w1"},{"id":"e3","source":"w1","target":"s2"}]}`
	_ = models.UpdateBotFlow(ctx, pool, bot.ID, json.RawMessage(flowJSON))

	inbound := func(waID, text string) []byte {
		return []byte(`{"entry":[{"changes":[{"value":{"metadata":{"phone_number_id":"PNIDF"},"messages":[{"from":"51999","id":"` + waID + `","type":"text","text":{"body":"` + text + `"}}]}}]}]}`)
	}

	con.processWhatsApp(inbound("wamid.f1", "hola"))
	con.processWhatsApp(inbound("wamid.f2", "algo"))

	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 || sent[0] != "Hola hola" || sent[1] != "fin" {
		t.Fatalf("secuencia del flujo incorrecta: %v", sent)
	}
}
