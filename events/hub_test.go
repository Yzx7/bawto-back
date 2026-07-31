package events

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

// Comprueba el camino completo: Publish → pg_notify → LISTEN → suscriptor.
// Se salta si no hay Postgres configurado (CI sin base).
func TestHubPublishReachesSubscriber(t *testing.T) {
	_ = godotenv.Load("../.env")
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("sin DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("sin conexión a Postgres: %v", err)
	}
	// El listener ocupa una conexión hasta que se cancele el contexto: hay que
	// cancelar antes de cerrar el pool o Close se queda esperando.
	defer func() {
		cancel()
		time.Sleep(200 * time.Millisecond)
		pool.Close()
	}()
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("sin conexión a Postgres: %v", err)
	}

	h := NewHub(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.Start(ctx)
	time.Sleep(500 * time.Millisecond) // que el LISTEN quede activo

	sub, unsub := h.Subscribe("bot-A")
	defer unsub()
	otro, unsubOtro := h.Subscribe("bot-B")
	defer unsubOtro()

	h.Publish(ctx, Event{Type: "message", BotID: "bot-A", ChatID: "chat-1", Payload: json.RawMessage(`{"id":1}`)})

	select {
	case e := <-sub:
		if e.Type != "message" || e.ChatID != "chat-1" {
			t.Fatalf("evento inesperado: %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("el suscriptor no recibió el evento")
	}

	// Otro bot no debe ver el evento.
	select {
	case e := <-otro:
		t.Fatalf("fuga entre bots: %+v", e)
	case <-time.After(300 * time.Millisecond):
	}
}
