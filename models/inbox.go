package models

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Bandeja de atención humana: listar conversaciones, leer historial paginado y
// resolver permisos. Todo cuelga del bot, y el bot de su organización.

// waWindow es la ventana de servicio de WhatsApp: fuera de ella Meta rechaza el
// texto libre y solo admite plantillas aprobadas.
const waWindow = 24 * time.Hour

// ChatSummary es una fila de la lista de conversaciones.
type ChatSummary struct {
	ID            string     `db:"id" json:"id"`
	Contact       string     `db:"contact" json:"contact"`
	ContactName   *string    `db:"contact_name" json:"contactName,omitempty"`
	Mode          string     `db:"mode" json:"mode"`
	HandoffUntil  *time.Time `db:"handoff_until" json:"handoffUntil,omitempty"`
	UpdatedAt     time.Time  `db:"updated_at" json:"updatedAt"`
	LastBody      *string    `db:"last_body" json:"lastBody,omitempty"`
	LastType      *string    `db:"last_type" json:"lastType,omitempty"`
	LastFromMe    *bool      `db:"last_from_me" json:"lastFromMe,omitempty"`
	LastAt        *time.Time `db:"last_at" json:"lastAt,omitempty"`
	LastInboundAt *time.Time `db:"last_inbound_at" json:"lastInboundAt,omitempty"`
	Unread        int        `db:"unread" json:"unread"`
}

// Message es un mensaje del historial tal como lo consume el panel.
type Message struct {
	ID                int64           `db:"id" json:"id"`
	WaID              *string         `db:"wa_id" json:"waId,omitempty"`
	FromMe            bool            `db:"from_me" json:"fromMe"`
	Type              string          `db:"type" json:"type"`
	Body              *string         `db:"body" json:"body,omitempty"`
	Metadata          json.RawMessage `db:"metadata" json:"metadata,omitempty"`
	HasMedia          bool            `db:"has_media" json:"hasMedia"`
	ProviderStatus    *string         `db:"provider_status" json:"providerStatus,omitempty"`
	ProviderStatusAt  *time.Time      `db:"provider_status_at" json:"providerStatusAt,omitempty"`
	ProviderErrorCode *string         `db:"provider_error_code" json:"providerErrorCode,omitempty"`
	ProviderError     *string         `db:"provider_error" json:"providerError,omitempty"`
	CreatedAt         time.Time       `db:"created_at" json:"createdAt"`
}

// ChatMeta reúne lo necesario para autorizar y para decidir si se puede escribir.
type ChatMeta struct {
	ID    string `db:"id" json:"id"`
	BotID string `db:"bot_id" json:"botId"`
	OrgID string `db:"org_id" json:"orgId"`
	// Contact es la identidad mostrable; ContactPhone y ContactUserID son las
	// dos formas reales de dirigirse a esa persona. Enviar necesita las dos
	// separadas: un BSUID puesto en `to` no da error, manda a otro sitio.
	Contact       string     `db:"contact" json:"contact"`
	ContactPhone  string     `db:"contact_phone" json:"contactPhone"`
	ContactUserID string     `db:"contact_user_id" json:"contactUserId"`
	ContactName   *string    `db:"contact_name" json:"contactName,omitempty"`
	Mode          string     `db:"mode" json:"mode"`
	HandoffUntil  *time.Time `db:"handoff_until" json:"handoffUntil,omitempty"`
	LastInboundAt *time.Time `db:"last_inbound_at" json:"lastInboundAt,omitempty"`
}

// WindowOpen dice si aún se puede enviar texto libre (ventana de 24 h de WhatsApp).
func (m *ChatMeta) WindowOpen() bool {
	return m.LastInboundAt != nil && time.Since(*m.LastInboundAt) < waWindow
}

// ListChats devuelve las conversaciones de un bot, más recientes primero.
// `before` pagina por `updated_at` (cero = primera página) y `q` filtra por
// número o nombre del contacto.
func ListChats(ctx context.Context, pool *pgxpool.Pool, botID, q string, before *time.Time, limit int) ([]ChatSummary, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	rows, err := pool.Query(ctx, `
		SELECT c.id::text AS id,
		       COALESCE(NULLIF(ct.phone_normalized,''), ct.channel_user_id, '') AS contact,
		       ct.name AS contact_name, c.mode, c.handoff_until, c.updated_at,
		       m.body AS last_body, m.type AS last_type, m.from_me AS last_from_me, m.created_at AS last_at,
		       (SELECT MAX(created_at) FROM messages i WHERE i.chat_id = c.id AND NOT i.from_me) AS last_inbound_at,
		       (SELECT COUNT(*) FROM messages u
		         WHERE u.chat_id = c.id AND NOT u.from_me
		           AND u.created_at > COALESCE(c.last_read_at, TIMESTAMPTZ 'epoch'))::int AS unread
		  FROM chats c
		  JOIN contacts ct ON ct.id = c.contact_id
		  LEFT JOIN LATERAL (
		        SELECT body, type, from_me, created_at FROM messages
		         WHERE chat_id = c.id ORDER BY created_at DESC, id DESC LIMIT 1
		  ) m ON TRUE
		 WHERE c.bot_id = $1::uuid
		   AND ($2 = '' OR COALESCE(ct.phone_normalized,'') ILIKE '%'||$2||'%'
		        OR COALESCE(ct.channel_user_id,'') ILIKE '%'||$2||'%'
		        OR COALESCE(ct.username,'') ILIKE '%'||$2||'%'
		        OR COALESCE(ct.name,'') ILIKE '%'||$2||'%')
		   AND ($3::timestamptz IS NULL OR c.updated_at < $3)
		 ORDER BY c.updated_at DESC
		 LIMIT $4`, botID, q, before, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ChatSummary])
}

// ListMessages devuelve el historial en orden cronológico. `before` (id) pagina
// hacia atrás: se piden los más recientes y se invierten al final.
func ListMessages(ctx context.Context, pool *pgxpool.Pool, chatID string, before int64, limit int) ([]Message, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := pool.Query(ctx, `
		SELECT m.id, m.wa_id, m.from_me, m.type, m.body, COALESCE(m.metadata,'{}'::jsonb) AS metadata,
		       (md.message_id IS NOT NULL) AS has_media, m.provider_status,
		       m.provider_status_at,m.provider_error_code,m.provider_error,m.created_at
		  FROM messages m
		  LEFT JOIN message_media md ON md.message_id = m.id
		 WHERE m.chat_id = $1::uuid AND ($2 = 0 OR m.id < $2)
		 ORDER BY m.id DESC
		 LIMIT $3`, chatID, before, limit)
	if err != nil {
		return nil, err
	}
	msgs, err := pgx.CollectRows(rows, pgx.RowToStructByName[Message])
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// GetChatMeta resuelve el chat junto a su bot y organización — nil si no existe.
func GetChatMeta(ctx context.Context, pool *pgxpool.Pool, chatID string) (*ChatMeta, error) {
	rows, err := pool.Query(ctx, `
		SELECT c.id::text AS id, c.bot_id::text AS bot_id, b.org_id::text AS org_id,
		       COALESCE(NULLIF(ct.phone_normalized,''), ct.channel_user_id, '') AS contact,
		       COALESCE(ct.phone_normalized,'') AS contact_phone,
		       COALESCE(ct.channel_user_id,'') AS contact_user_id,
		       ct.name AS contact_name, c.mode, c.handoff_until,
		       (SELECT MAX(created_at) FROM messages i WHERE i.chat_id = c.id AND NOT i.from_me) AS last_inbound_at
		  FROM chats c
		  JOIN bots b ON b.id = c.bot_id
		  JOIN contacts ct ON ct.id = c.contact_id
		 WHERE c.id = $1::uuid`, chatID)
	if err != nil {
		return nil, err
	}
	m, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[ChatMeta])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// MarkChatRead pone el corte de "no leídos" en ahora.
func MarkChatRead(ctx context.Context, pool *pgxpool.Pool, chatID string) error {
	_, err := pool.Exec(ctx, `UPDATE chats SET last_read_at = NOW() WHERE id = $1::uuid`, chatID)
	return err
}

// InsertOutboundMessage guarda un envío nuestro y devuelve la fila completa,
// para publicarla al instante por SSE sin una consulta extra. Devuelve nil sin
// error si el `wa_id` ya estaba guardado (el eco de Coexistence puede ganarnos
// la carrera): en ese caso no hay nada nuevo que anunciar.
func InsertOutboundMessage(ctx context.Context, pool *pgxpool.Pool, chatID, waID, typ, body string) (*Message, error) {
	return InsertOutboundMessageWithMetadata(ctx, pool, chatID, waID, typ, body, nil)
}

// InsertOutboundMessageWithMetadata conserva la trazabilidad del scheduler
// (flow/version/run/registro/plantilla) junto al mensaje visible en la bandeja.
func InsertOutboundMessageWithMetadata(ctx context.Context, pool *pgxpool.Pool, chatID, waID, typ, body string, metadata json.RawMessage) (*Message, error) {
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	rows, err := pool.Query(ctx, `
		INSERT INTO messages (chat_id, wa_id, from_me, type, body, metadata, provider_status, provider_status_at)
		VALUES ($1::uuid, NULLIF($2,''), TRUE, $3, $4, $5::jsonb,
		        CASE WHEN $2<>'' THEN 'sent' ELSE NULL END,
		        CASE WHEN $2<>'' THEN NOW() ELSE NULL END)
		ON CONFLICT (wa_id) WHERE wa_id IS NOT NULL DO NOTHING
		RETURNING id, wa_id, from_me, type, body, COALESCE(metadata,'{}'::jsonb) AS metadata,
		          FALSE AS has_media, provider_status,provider_status_at,
		          provider_error_code,provider_error,created_at`, chatID, waID, typ, body, metadata)
	if err != nil {
		return nil, err
	}
	m, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Message])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_, _ = pool.Exec(ctx, `UPDATE chats SET updated_at = NOW() WHERE id = $1::uuid`, chatID)
	return &m, nil
}

// MessageMedia es una copia durable del archivo (los media_id de Meta expiran).
type MessageMedia struct {
	MessageID  int64  `db:"message_id"`
	ProviderID string `db:"provider_id"`
	MimeType   string `db:"mime_type"`
	Data       []byte `db:"data"`
	BotID      string `db:"bot_id"`
	OrgID      string `db:"org_id"`
}

// GetMessageMedia trae el archivo junto a la org dueña, para autorizar.
func GetMessageMedia(ctx context.Context, pool *pgxpool.Pool, messageID int64) (*MessageMedia, error) {
	rows, err := pool.Query(ctx, `
		SELECT md.message_id,md.provider_id,md.mime_type,md.data,
		       b.id::text AS bot_id,b.org_id::text AS org_id
		  FROM message_media md
		  JOIN messages m ON m.id = md.message_id
		  JOIN chats c ON c.id = m.chat_id
		  JOIN bots b ON b.id = c.bot_id
		 WHERE md.message_id = $1`, messageID)
	if err != nil {
		return nil, err
	}
	md, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[MessageMedia])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &md, nil
}
