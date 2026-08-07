package controllers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"

	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yzx7/sacs-chatbots/config"
	"github.com/Yzx7/sacs-chatbots/env"
	"github.com/Yzx7/sacs-chatbots/helpers"
	"github.com/Yzx7/sacs-chatbots/models"
)

// Un eco que llega mientras el bot todavía está guardando su propio envío no
// debe interpretarse como que escribió una persona.
//
// El motor manda el mensaje a Meta y solo después persiste su wa_id. En ese
// hueco `MessageExists` responde que no es nuestro, así que sin el advisory lock
// el eco provoca un handoff y **el bot se silencia a sí mismo 12 h**, sin dejar
// ningún error. El test reproduce el hueco a propósito: toma el lock del chat
// como lo haría el envío en vuelo, lanza el eco y solo entonces persiste.
func TestEcoNoSilenciaAlBotDuranteSuPropioEnvio(t *testing.T) {
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

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cph, _ := helpers.NewCipher(hex.EncodeToString(key))
	con := New(&env.Env{
		Postgres: pool,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Cipher:   cph,
		Config:   &config.Config{WhatsAppAppSecret: "appsecret"},
	})

	uid := randHex("echou_")
	if _, err := pool.Exec(ctx, `INSERT INTO "user"(id,name,email,"emailVerified") VALUES ($1,$2,$3,false)`,
		uid, "Echo Owner", uid+"@test.local"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, uid)

	org, err := models.CreateOrganization(ctx, pool, uid, "Echo Org", nil, nil)
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	defer models.DeleteOrganization(ctx, pool, org.ID)

	bot, err := models.CreateBot(ctx, pool, org.ID, "Echo Bot", "wsp")
	if err != nil {
		t.Fatalf("CreateBot: %v", err)
	}
	enc, _ := cph.Encrypt("TESTTOKEN")
	if err := models.UpdateBotChannel(ctx, pool, bot.ID, "wsp", "PNIDECHO", "51900", enc); err != nil {
		t.Fatalf("UpdateBotChannel: %v", err)
	}

	const contact = "51999111222"
	// Único por corrida: `messages.wa_id` es único global, así que un valor fijo
	// sobrevive a la limpieza y hace que MessageExists diga «ya existe» desde el
	// primer instante. El test pasaría siempre, incluso sin el lock.
	waID := randHex("wamid.echopropio_")
	chatID, err := models.UpsertChat(ctx, pool, bot.ID, contact, "")
	if err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}

	payload := []byte(`{"entry":[{"changes":[{"field":"smb_message_echoes","value":{` +
		`"metadata":{"phone_number_id":"PNIDECHO"},` +
		`"message_echoes":[{"to":"` + contact + `","id":"` + waID + `","type":"text",` +
		`"text":{"body":"respuesta del bot"}}]}}]}]}`)

	// El envío está en vuelo: el motor tiene el lock del chat y aún no persistió.
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Con `defer`, y no a mano al final: si el test falla antes de soltar la
	// conexión, `pool.Close()` se queda esperándola para siempre y el fallo se
	// convierte en un cuelgue. El unlock va después para que corra antes (LIFO):
	// un advisory lock es de sesión, así que devolver la conexión al pool con el
	// lock puesto lo dejaría tomado para quien la reciba luego.
	defer lockConn.Release()
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, chatID); err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1,0))`, chatID)

	done := make(chan struct{})
	go func() {
		defer close(done)
		con.handleEchoes(ctx, payload)
	}()

	// Discriminante del test. No se apuesta a ganar una carrera con un sleep
	// corto: contra un Postgres remoto, `GetBotByChannel` y `UpsertChat` tardan
	// más que cualquier margen razonable y el eco llegaría a decidir *después*
	// de que el envío ya persistió, con lo que el test pasaría sin el lock.
	// Se comprueba el invariante directamente: mientras el envío tenga el lock,
	// el eco **no puede haber terminado**. Sin el arreglo termina de inmediato.
	select {
	case <-done:
		t.Fatal("el eco decidió sin esperar al lock del chat: la carrera sigue abierta")
	case <-time.After(6 * time.Second):
	}

	// El envío termina: recién ahora existe el wa_id en la base.
	if _, err := models.InsertOutboundMessage(ctx, pool, chatID, waID, "text", "respuesta del bot"); err != nil {
		t.Fatalf("InsertOutboundMessage: %v", err)
	}
	_, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1,0))`, chatID)

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("handleEchoes no terminó: el lock no se liberó")
	}

	var mode string
	if err := pool.QueryRow(ctx, `SELECT mode FROM chats WHERE id = $1::uuid`, chatID).Scan(&mode); err != nil {
		t.Fatalf("select mode: %v", err)
	}
	if mode != "bot" {
		t.Fatalf("el bot se silenció con su propio eco: mode=%q, esperaba \"bot\"", mode)
	}

	var salientes int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE chat_id = $1::uuid AND wa_id = $2`,
		chatID, waID).Scan(&salientes); err != nil {
		t.Fatalf("count: %v", err)
	}
	if salientes != 1 {
		t.Fatalf("esperaba 1 fila para el wa_id propio, got %d", salientes)
	}
}

// El eco de una persona escribiendo desde la app de WhatsApp Business sí debe
// silenciar al bot. Es el otro lado del test anterior: el lock no puede volver
// insensible al handoff, que es la razón de ser del campo.
func TestEcoDeUnHumanoSilenciaAlBot(t *testing.T) {
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

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cph, _ := helpers.NewCipher(hex.EncodeToString(key))
	con := New(&env.Env{
		Postgres: pool,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Cipher:   cph,
		Config:   &config.Config{WhatsAppAppSecret: "appsecret"},
	})

	uid := randHex("echoh_")
	if _, err := pool.Exec(ctx, `INSERT INTO "user"(id,name,email,"emailVerified") VALUES ($1,$2,$3,false)`,
		uid, "Echo Owner", uid+"@test.local"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, uid)

	org, err := models.CreateOrganization(ctx, pool, uid, "Echo Org H", nil, nil)
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	defer models.DeleteOrganization(ctx, pool, org.ID)

	bot, err := models.CreateBot(ctx, pool, org.ID, "Echo Bot H", "wsp")
	if err != nil {
		t.Fatalf("CreateBot: %v", err)
	}
	enc, _ := cph.Encrypt("TESTTOKEN")
	if err := models.UpdateBotChannel(ctx, pool, bot.ID, "wsp", "PNIDECHOH", "51900", enc); err != nil {
		t.Fatalf("UpdateBotChannel: %v", err)
	}

	const contact = "51999333444"
	payload := []byte(`{"entry":[{"changes":[{"field":"smb_message_echoes","value":{` +
		`"metadata":{"phone_number_id":"PNIDECHOH"},` +
		`"message_echoes":[{"to":"` + contact + `","id":"` + randHex("wamid.humano_") + `","type":"text",` +
		`"text":{"body":"te atiendo yo"}}]}}]}]}`)

	con.handleEchoes(ctx, payload)

	chatID, err := models.UpsertChat(ctx, pool, bot.ID, contact, "")
	if err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	var mode string
	var handoffUntil *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT mode, handoff_until FROM chats WHERE id = $1::uuid`, chatID).Scan(&mode, &handoffUntil); err != nil {
		t.Fatalf("select mode: %v", err)
	}
	if mode != "manual" {
		t.Fatalf("el eco humano no silenció al bot: mode=%q", mode)
	}
	if handoffUntil == nil || !handoffUntil.After(time.Now()) {
		t.Fatalf("handoff_until no quedó en el futuro: %v", handoffUntil)
	}
}
