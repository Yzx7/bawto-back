package models

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Bot struct {
	ID        string          `db:"id" json:"id"`
	OrgID     string          `db:"org_id" json:"orgId"`
	Name      string          `db:"name" json:"name"`
	Channel   string          `db:"channel" json:"channel"`
	ChannelID *string         `db:"channel_id" json:"channelId,omitempty"`
	Phone     *string         `db:"phone" json:"phone,omitempty"`
	Flow      json.RawMessage `db:"flow" json:"flow"`
	AIConfig  json.RawMessage `db:"ai_config" json:"aiConfig,omitempty"`
	CreatedAt time.Time       `db:"created_at" json:"createdAt"`
	UpdatedAt time.Time       `db:"updated_at" json:"updatedAt"`
}

// Columnas públicas (excluye secretos: token_enc, verify_token).
const botCols = `id::text AS id, org_id::text AS org_id, name, channel, channel_id, phone, flow, ai_config, created_at, updated_at`

// ListBotsByOrg devuelve los bots de una organización.
func ListBotsByOrg(ctx context.Context, pool *pgxpool.Pool, orgID string) ([]Bot, error) {
	rows, err := pool.Query(ctx,
		`SELECT `+botCols+` FROM bots WHERE org_id = $1::uuid ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Bot])
}

// GetBot devuelve un bot por id (nil si no existe).
func GetBot(ctx context.Context, pool *pgxpool.Pool, botID string) (*Bot, error) {
	rows, err := pool.Query(ctx, `SELECT `+botCols+` FROM bots WHERE id = $1::uuid`, botID)
	if err != nil {
		return nil, err
	}
	b, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Bot])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// CreateBot crea un bot en una organización (sin canal conectado aún).
func CreateBot(ctx context.Context, pool *pgxpool.Pool, orgID, name, channel string) (*Bot, error) {
	if channel == "" {
		channel = "wsp"
	}
	rows, err := pool.Query(ctx,
		`INSERT INTO bots (org_id, name, channel) VALUES ($1::uuid, $2, $3)
		 RETURNING `+botCols, orgID, name, channel)
	if err != nil {
		return nil, err
	}
	b, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Bot])
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// UpdateBotName renombra un bot.
func UpdateBotName(ctx context.Context, pool *pgxpool.Pool, botID, name string) error {
	_, err := pool.Exec(ctx, `UPDATE bots SET name = $2 WHERE id = $1::uuid`, botID, name)
	return err
}

// UpdateBotFlow guarda el grafo del flujo (JSONB).
func UpdateBotFlow(ctx context.Context, pool *pgxpool.Pool, botID string, flow json.RawMessage) error {
	_, err := pool.Exec(ctx, `UPDATE bots SET flow = $2 WHERE id = $1::uuid`, botID, flow)
	return err
}

// DeleteBot elimina un bot (chats/messages/knowledge caen por FK cascade).
func DeleteBot(ctx context.Context, pool *pgxpool.Pool, botID string) error {
	_, err := pool.Exec(ctx, `DELETE FROM bots WHERE id = $1::uuid`, botID)
	return err
}

// BotChannel incluye el secreto del canal y el flujo (para el webhook).
type BotChannel struct {
	ID        string          `db:"id"`
	OrgID     string          `db:"org_id"`
	Channel   string          `db:"channel"`
	ChannelID *string         `db:"channel_id"`
	Phone     *string         `db:"phone"`
	TokenEnc  []byte          `db:"token_enc"`
	Flow      json.RawMessage `db:"flow"`
}

// UpdateBotChannel conecta un bot a un canal (phone_number_id + phone + token cifrado).
func UpdateBotChannel(ctx context.Context, pool *pgxpool.Pool, botID, channel, channelID, phone string, tokenEnc []byte) error {
	_, err := pool.Exec(ctx,
		`UPDATE bots SET channel = $2, channel_id = $3, phone = NULLIF($4,''), token_enc = $5
		 WHERE id = $1::uuid`, botID, channel, channelID, phone, tokenEnc)
	return err
}

// GetBotByChannel busca el bot dueño de un channel_id (phone_number_id) — nil si no existe.
func GetBotByChannel(ctx context.Context, pool *pgxpool.Pool, channel, channelID string) (*BotChannel, error) {
	rows, err := pool.Query(ctx,
		`SELECT id::text AS id, org_id::text AS org_id, channel, channel_id, phone, token_enc, flow
		 FROM bots WHERE channel = $1 AND channel_id = $2`, channel, channelID)
	if err != nil {
		return nil, err
	}
	b, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[BotChannel])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}
