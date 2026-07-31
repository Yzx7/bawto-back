// Package events distribuye eventos de chat en vivo hacia los clientes SSE.
//
// El transporte es Postgres (`LISTEN/NOTIFY`), no memoria: el webhook, el
// scheduler y la API pueden correr en procesos distintos y todos publican al
// mismo canal. Cada proceso mantiene una conexión escuchando y hace fan-out a
// sus suscriptores locales. Sin Redis ni servidor de sockets aparte.
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// channel es el canal de NOTIFY que usamos para todo el tráfico de chats.
const channel = "chat_events"

// bodyLimit recorta el cuerpo publicado: el payload de NOTIFY tope es 8000 bytes.
const bodyLimit = 900

// Event es lo que viaja al navegador. Payload depende de Type:
//   - "message": el mensaje recién guardado (entrante o saliente)
//   - "mode":    el chat cambió entre bot y manual
type Event struct {
	Type    string          `json:"type"`
	BotID   string          `json:"botId"`
	ChatID  string          `json:"chatId"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Hub reparte los eventos recibidos de Postgres entre los suscriptores locales.
type Hub struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	mu   sync.RWMutex
	subs map[chan Event]string // suscriptor → botID que le interesa
}

func NewHub(pool *pgxpool.Pool, log *slog.Logger) *Hub {
	return &Hub{pool: pool, log: log, subs: map[chan Event]string{}}
}

// Subscribe entrega un canal con los eventos de un bot y la función para cerrarlo.
// El buffer evita que un cliente lento bloquee el fan-out: si se llena, se descarta
// el evento (el cliente recupera el estado real al recargar la conversación).
func (h *Hub) Subscribe(botID string) (<-chan Event, func()) {
	ch := make(chan Event, 32)
	h.mu.Lock()
	h.subs[ch] = botID
	h.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// Publish emite el evento a todos los procesos. Nunca es crítico: si falla, el
// cliente igual verá el mensaje en el siguiente refresco.
func (h *Hub) Publish(ctx context.Context, e Event) {
	if h == nil || h.pool == nil {
		return
	}
	if len(e.Payload) > bodyLimit {
		e.Payload = nil // demasiado grande para NOTIFY: que el cliente recargue
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	if _, err := h.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, channel, string(raw)); err != nil {
		h.log.Warn("events publish", "err", err.Error())
	}
}

// Start deja una goroutine escuchando el canal de Postgres, con reconexión.
func (h *Hub) Start(ctx context.Context) {
	go func() {
		for ctx.Err() == nil {
			if err := h.listen(ctx); err != nil && ctx.Err() == nil {
				h.log.Warn("events listen caído, reintentando", "err", err.Error())
				time.Sleep(3 * time.Second)
			}
		}
	}()
}

// listen ocupa una conexión dedicada del pool hasta que falle o se cancele.
func (h *Hub) listen(ctx context.Context) error {
	conn, err := h.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return err
	}
	h.log.Info("events: escuchando", "channel", channel)

	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var e Event
		if json.Unmarshal([]byte(n.Payload), &e) == nil {
			h.fanout(e)
		}
	}
}

func (h *Hub) fanout(e Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch, botID := range h.subs {
		if botID != e.BotID {
			continue
		}
		select {
		case ch <- e:
		default: // suscriptor saturado: se descarta
		}
	}
}
