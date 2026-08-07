package models

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yzx7/sacs-chatbots/channels/whatsapp"
)

// ContactPreference es la preferencia vigente de un contacto para una categoría.
// Los valores son los de Meta, sin traducir.
type ContactPreference struct {
	ContactID  string    `db:"contact_id"  json:"contactId"`
	Category   string    `db:"category"    json:"category"`
	Value      string    `db:"value"       json:"value"`
	Detail     *string   `db:"detail"      json:"detail,omitempty"`
	OccurredAt time.Time `db:"occurred_at" json:"occurredAt"`
	UpdatedAt  time.Time `db:"updated_at"  json:"updatedAt"`
}

// StoreAndApplyUserPreference guarda el cambio y actualiza la preferencia
// vigente. Devuelve false si ya se había aplicado.
//
// La guarda de orden importa más aquí que en ningún otro sitio: un `resume`
// atrasado que pisara un `stop` reciente volvería a habilitar envíos de
// marketing a quien ya dijo que no.
func StoreAndApplyUserPreference(
	ctx context.Context,
	pool *pgxpool.Pool,
	contactID string,
	pref whatsapp.UserPreference,
) (bool, error) {
	if pref.EventKey == "" || contactID == "" || pref.Value == "" {
		return false, errors.New("preferencia incompleta")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var eventID int64
	err = tx.QueryRow(ctx, `INSERT INTO contact_preference_events(
			event_key,contact_id,category,value,detail,occurred_at,payload)
		VALUES($1,$2::uuid,$3,$4,NULLIF($5,''),$6,$7::jsonb)
		ON CONFLICT(event_key) DO NOTHING RETURNING id`,
		pref.EventKey, contactID, pref.Category, pref.Value, pref.Detail,
		pref.OccurredAt, pref.Payload).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	_, err = tx.Exec(ctx, `INSERT INTO contact_preferences(
			contact_id,category,value,detail,occurred_at,updated_at)
		VALUES($1::uuid,$2,$3,NULLIF($4,''),$5,NOW())
		ON CONFLICT(contact_id,category) DO UPDATE SET
			value=EXCLUDED.value,
			detail=EXCLUDED.detail,
			occurred_at=EXCLUDED.occurred_at,
			updated_at=NOW()
		WHERE EXCLUDED.occurred_at >= contact_preferences.occurred_at`,
		contactID, pref.Category, pref.Value, pref.Detail, pref.OccurredAt)
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE contact_preference_events SET applied_at=NOW() WHERE id=$1`, eventID); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// MarketingOptedOut indica si el contacto pidió no recibir marketing.
//
// Se consulta **antes de encolar**, no después: Meta cobra por plantilla
// entregada, así que filtrar tarde es pagar por un envío indebido.
func MarketingOptedOut(ctx context.Context, pool *pgxpool.Pool, contactID string) (bool, error) {
	var value string
	err := pool.QueryRow(ctx, `
		SELECT value FROM contact_preferences
		WHERE contact_id = $1::uuid AND category = $2`,
		contactID, whatsapp.PreferenceCategoryMarketing).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return value == whatsapp.PreferenceStop, nil
}

// ListContactPreferences devuelve las preferencias vigentes de un contacto, para
// que quien opera entienda por qué alguien quedó fuera de un envío.
func ListContactPreferences(ctx context.Context, pool *pgxpool.Pool, contactID string) ([]ContactPreference, error) {
	rows, err := pool.Query(ctx, `
		SELECT contact_id,category,value,detail,occurred_at,updated_at
		FROM contact_preferences WHERE contact_id = $1::uuid ORDER BY category`, contactID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ContactPreference])
}
