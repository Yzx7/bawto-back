package models

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UpsertChat crea (o actualiza) el chat de un contacto en un bot; devuelve su id.
//
// La clave es el contacto, no una cadena de teléfono: desde los nombres de
// usuario de WhatsApp la misma persona puede llegar con teléfono un día y solo
// con su BSUID al siguiente, y con la clave textual eso abría un chat nuevo
// —o, peor, uno con identidad vacía compartido por todos los desconocidos—.
func UpsertChat(ctx context.Context, pool *pgxpool.Pool, botID, contactID string) (string, error) {
	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO chats (bot_id, contact_id)
		 VALUES ($1::uuid, $2::uuid)
		 ON CONFLICT (bot_id, contact_id) DO UPDATE SET updated_at = NOW()
		 RETURNING id::text`, botID, contactID).Scan(&id)
	return id, err
}

// InsertMessage guarda un mensaje (idempotente por wa_id cuando existe).
func InsertMessage(ctx context.Context, pool *pgxpool.Pool, chatID, waID string, fromMe bool, typ, body string) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO messages (chat_id, wa_id, from_me, type, body)
		 VALUES ($1::uuid, NULLIF($2,''), $3, $4, $5)
		 ON CONFLICT (wa_id) WHERE wa_id IS NOT NULL DO NOTHING`,
		chatID, waID, fromMe, typ, body)
	return err
}

// InsertInboundMessage guarda metadatos del canal y devuelve false si el wa_id
// ya había sido procesado. El caller no debe volver a ejecutar el flujo.
func InsertInboundMessage(ctx context.Context, pool *pgxpool.Pool, chatID, waID, typ, body string, metadata json.RawMessage) (int64, bool, error) {
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO messages (chat_id,wa_id,from_me,type,body,metadata)
		VALUES ($1::uuid,NULLIF($2,''),false,$3,$4,$5::jsonb)
		ON CONFLICT (wa_id) WHERE wa_id IS NOT NULL DO NOTHING RETURNING id`, chatID, waID, typ, body, metadata).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// HasNewerInboundText detecta si este mensaje forma parte de una ráfaga que ya
// tiene otro fragmento textual posterior. Se compara por id —orden de inserción
// durable— y no por el orden impredecible en que las goroutines ganan el lock.
//
// El caller solo debe absorber texto/interacciones. Una imagen nunca se agrupa:
// el mensaje actual es el que autoriza al nodo visual a leer sus bytes.
func HasNewerInboundText(ctx context.Context, pool *pgxpool.Pool, chatID string, messageID int64, window time.Duration) (bool, error) {
	var newer bool
	err := pool.QueryRow(ctx, `
		WITH current_message AS (
			SELECT created_at FROM messages WHERE id=$2 AND chat_id=$1::uuid
		)
		SELECT EXISTS (
			SELECT 1
			  FROM messages newer, current_message current
			 WHERE newer.chat_id=$1::uuid
			   AND NOT newer.from_me
			   AND newer.id>$2
			   AND newer.type IN ('text','interactive','reply')
			   AND newer.created_at <= current.created_at + ($3 * interval '1 millisecond')
		)`, chatID, messageID, window.Milliseconds()).Scan(&newer)
	return newer, err
}

// InboundTextBurst devuelve los fragmentos textuales que pertenecen al turno
// representado por messageID. Un mensaje saliente actúa como frontera: nunca se
// vuelve a inyectar texto que el bot ya contestó en un turno anterior.
func InboundTextBurst(ctx context.Context, pool *pgxpool.Pool, chatID string, messageID int64, window time.Duration) ([]string, error) {
	rows, err := pool.Query(ctx, `
		WITH current_message AS (
			SELECT created_at FROM messages WHERE id=$2 AND chat_id=$1::uuid
		)
		SELECT COALESCE(fragment.body,'')
		  FROM messages fragment, current_message current
		 WHERE fragment.chat_id=$1::uuid
		   AND NOT fragment.from_me
		   AND fragment.id<=$2
		   AND fragment.type IN ('text','interactive','reply')
		   AND fragment.created_at >= current.created_at - ($3 * interval '1 millisecond')
		   AND NOT EXISTS (
			 SELECT 1 FROM messages response
			  WHERE response.chat_id=fragment.chat_id
			    AND response.from_me
			    AND response.id>fragment.id
			    AND response.id<=$2
		   )
		 ORDER BY fragment.id
		 LIMIT 20`, chatID, messageID, window.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var fragments []string
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		if body = strings.TrimSpace(body); body != "" {
			fragments = append(fragments, body)
		}
	}
	return fragments, rows.Err()
}

func SaveMessageMedia(ctx context.Context, pool *pgxpool.Pool, messageID int64, providerID, mimeType string, data []byte) error {
	_, err := pool.Exec(ctx, `INSERT INTO message_media(message_id,provider_id,mime_type,data)
		VALUES ($1,$2,$3,$4) ON CONFLICT(message_id) DO UPDATE SET provider_id=EXCLUDED.provider_id,mime_type=EXCLUDED.mime_type,data=EXCLUDED.data`, messageID, providerID, mimeType, data)
	return err
}

// GetChatState devuelve el estado de conversación del motor (chats.current_layer).
func GetChatState(ctx context.Context, pool *pgxpool.Pool, chatID string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := pool.QueryRow(ctx, `SELECT current_layer FROM chats WHERE id = $1::uuid`, chatID).Scan(&raw)
	return raw, err
}

// SetChatState persiste el estado del motor (o "null" al terminar).
func SetChatState(ctx context.Context, pool *pgxpool.Pool, chatID string, state json.RawMessage) error {
	_, err := pool.Exec(ctx,
		`UPDATE chats SET current_layer = $2, updated_at = NOW() WHERE id = $1::uuid`, chatID, state)
	return err
}

// ---- Modo de atención: bot vs humano (handoff) ----
//
// Clave con Coexistence: el mismo número lo usan el bot (API) y una persona
// (app de WhatsApp Business). Si un humano toma la conversación, el bot calla.

// GetChatMode devuelve el modo del chat y hasta cuándo dura el handoff (nil = indefinido).
func GetChatMode(ctx context.Context, pool *pgxpool.Pool, chatID string) (string, *time.Time, error) {
	var mode string
	var until *time.Time
	err := pool.QueryRow(ctx,
		`SELECT mode, handoff_until FROM chats WHERE id = $1::uuid`, chatID).Scan(&mode, &until)
	return mode, until, err
}

// SetChatMode cambia el modo ("bot" | "manual"); until nil = sin vencimiento.
func SetChatMode(ctx context.Context, pool *pgxpool.Pool, chatID, mode string, until *time.Time) error {
	_, err := pool.Exec(ctx,
		`UPDATE chats SET mode = $2, handoff_until = $3, updated_at = NOW() WHERE id = $1::uuid`,
		chatID, mode, until)
	return err
}

// HandoffChat pasa el chat a un humano por `d` (0 = indefinido).
func HandoffChat(ctx context.Context, pool *pgxpool.Pool, chatID string, d time.Duration) error {
	var until *time.Time
	if d > 0 {
		t := time.Now().Add(d)
		until = &t
	}
	return SetChatMode(ctx, pool, chatID, "manual", until)
}

// BotSilenced indica si el bot debe callarse en este chat. Si el handoff venció,
// devuelve el chat a modo bot y responde false.
func BotSilenced(ctx context.Context, pool *pgxpool.Pool, chatID string) bool {
	mode, until, err := GetChatMode(ctx, pool, chatID)
	if err != nil || mode != "manual" {
		return false
	}
	if until != nil && time.Now().After(*until) {
		_ = SetChatMode(ctx, pool, chatID, "bot", nil)
		return false
	}
	return true
}

// MessageExists dice si ya guardamos un mensaje con ese wa_id. Sirve para los ecos
// de Coexistence: si el eco NO existe, lo escribió una persona desde la app.
func MessageExists(ctx context.Context, pool *pgxpool.Pool, waID string) (bool, error) {
	if waID == "" {
		return false, nil
	}
	var one int
	err := pool.QueryRow(ctx, `SELECT 1 FROM messages WHERE wa_id = $1 LIMIT 1`, waID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
