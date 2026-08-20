package models

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yzx7/sacs-chatbots/channels/whatsapp"
)

// ChannelHealth es el estado actual del canal proyectado desde los eventos de
// cuenta. Los valores vienen tal cual de Meta; no se traducen.
type ChannelHealth struct {
	WabaID         string     `db:"waba_id"           json:"wabaId"`
	PhoneNumberID  *string    `db:"phone_number_id"   json:"phoneNumberId,omitempty"`
	DisplayPhone   *string    `db:"display_phone"     json:"displayPhone,omitempty"`
	QualityEvent   *string    `db:"quality_event"     json:"qualityEvent,omitempty"`
	MessagingLimit *string    `db:"messaging_limit"   json:"messagingLimit,omitempty"`
	AccountEvent   *string    `db:"account_event"     json:"accountEvent,omitempty"`
	ReviewDecision *string    `db:"review_decision"   json:"reviewDecision,omitempty"`
	NameDecision   *string    `db:"name_decision"     json:"nameDecision,omitempty"`
	Severity       string     `db:"severity"          json:"severity"`
	LastEventField *string    `db:"last_event_field"  json:"lastEventField,omitempty"`
	LastEventAt    *time.Time `db:"last_event_at"     json:"lastEventAt,omitempty"`
	UpdatedAt      time.Time  `db:"updated_at"        json:"updatedAt"`
}

// ChannelAccountEvent es una fila de la bitácora, para el historial del panel.
type ChannelAccountEvent struct {
	ID            int64           `db:"id"              json:"id"`
	WabaID        string          `db:"waba_id"         json:"wabaId"`
	PhoneNumberID *string         `db:"phone_number_id" json:"phoneNumberId,omitempty"`
	DisplayPhone  *string         `db:"display_phone"   json:"displayPhone,omitempty"`
	Field         string          `db:"field"           json:"field"`
	Severity      string          `db:"severity"        json:"severity"`
	OccurredAt    time.Time       `db:"occurred_at"     json:"occurredAt"`
	Payload       json.RawMessage `db:"payload"         json:"payload"`
}

// StoreAndApplyAccountEvent guarda el evento en la bitácora y proyecta el estado.
// Devuelve false si el evento ya se había aplicado (Meta reintenta).
//
// Cada campo escribe **solo sus columnas**, igual que en plantillas: un
// phone_number_quality_update no debe borrar lo que dejó un account_update. El
// last_event_at impide que un webhook atrasado revierta un estado más nuevo, que
// con Meta no es hipotético: los eventos no llegan ordenados.
func StoreAndApplyAccountEvent(
	ctx context.Context,
	pool *pgxpool.Pool,
	event whatsapp.AccountEvent,
) (bool, error) {
	if event.EventKey == "" || event.WabaID == "" || event.Field == "" {
		return false, errors.New("evento de cuenta incompleto")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	// Meta identifica el número por su display phone, no por el phone_number_id
	// con el que trabaja el resto del sistema. Se resuelve contra nuestros propios
	// bots: es el único sitio donde esa correspondencia existe. Si no resuelve
	// —por ejemplo en un payload de prueba— se conserva el número tal cual y el
	// evento sigue siendo suyo, no de la cuenta.
	phoneNumberID := event.PhoneNumberID
	if phoneNumberID == "" && event.DisplayPhone != "" {
		if resolved, err := resolvePhoneNumberID(ctx, tx, event.WabaID, event.DisplayPhone); err == nil {
			phoneNumberID = resolved
		}
	}

	var eventID int64
	err = tx.QueryRow(ctx, `INSERT INTO channel_account_events(
			event_key,waba_id,phone_number_id,display_phone,field,severity,occurred_at,payload)
		VALUES($1,$2,NULLIF($3,''),NULLIF($4,''),$5,$6,$7,$8::jsonb)
		ON CONFLICT(event_key) DO NOTHING RETURNING id`,
		event.EventKey, event.WabaID, phoneNumberID, event.DisplayPhone, event.Field,
		event.Severity, event.OccurredAt, event.Payload).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	quality, limit, accountEvent, review, name := "", "", "", "", ""
	switch event.Field {
	case "phone_number_quality_update":
		quality, limit = event.Event, event.MessagingLimit
	case "account_update":
		accountEvent = event.Event
	case "account_review_update":
		review = event.Decision
	case "phone_number_name_update":
		name = event.Decision
	}

	_, err = tx.Exec(ctx, `INSERT INTO channel_health(
			waba_id,phone_number_id,display_phone,quality_event,messaging_limit,account_event,
			review_decision,name_decision,severity,last_event_field,last_event_at,updated_at)
		VALUES($1,NULLIF($2,''),NULLIF($11,''),NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),
			NULLIF($6,''),NULLIF($7,''),$8,$9,$10,NOW())
		ON CONFLICT(waba_id,scope) DO UPDATE SET
			display_phone=COALESCE(EXCLUDED.display_phone,channel_health.display_phone),
			quality_event=COALESCE(EXCLUDED.quality_event,channel_health.quality_event),
			messaging_limit=COALESCE(EXCLUDED.messaging_limit,channel_health.messaging_limit),
			account_event=COALESCE(EXCLUDED.account_event,channel_health.account_event),
			review_decision=COALESCE(EXCLUDED.review_decision,channel_health.review_decision),
			name_decision=COALESCE(EXCLUDED.name_decision,channel_health.name_decision),
			severity=EXCLUDED.severity,
			last_event_field=EXCLUDED.last_event_field,
			last_event_at=EXCLUDED.last_event_at,
			updated_at=NOW()
		WHERE channel_health.last_event_at IS NULL
			OR EXCLUDED.last_event_at >= channel_health.last_event_at`,
		event.WabaID, phoneNumberID, quality, limit, accountEvent,
		review, name, event.Severity, event.Field, event.OccurredAt, event.DisplayPhone)
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE channel_account_events SET applied_at=NOW() WHERE id=$1`, eventID); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// resolvePhoneNumberID traduce el número que escribe Meta al phone_number_id con
// el que trabaja el resto del sistema, buscándolo entre los bots del WABA. La
// comparación es sobre dígitos: Meta manda "16505551111" y el bot puede tener
// guardado "+1 650 555 1111".
func resolvePhoneNumberID(ctx context.Context, tx pgx.Tx, wabaID, displayPhone string) (string, error) {
	digits := NormalizePhone(displayPhone)
	if digits == "" {
		return "", errors.New("número vacío")
	}
	var channelID string
	err := tx.QueryRow(ctx, `
		SELECT channel_id FROM bots
		WHERE waba_id = $1 AND channel_id IS NOT NULL
		  AND regexp_replace(COALESCE(phone,''), '\D', '', 'g') = $2
		LIMIT 1`, wabaID, digits).Scan(&channelID)
	if err != nil {
		return "", err
	}
	return channelID, nil
}

// GetChannelHealth devuelve las filas de salud de un WABA: la de la cuenta
// (sin número) y una por número con eventos.
func GetChannelHealth(ctx context.Context, pool *pgxpool.Pool, wabaID string) ([]ChannelHealth, error) {
	rows, err := pool.Query(ctx, `
		SELECT waba_id,phone_number_id,display_phone,quality_event,messaging_limit,account_event,
		       review_decision,name_decision,severity,last_event_field,last_event_at,updated_at
		FROM channel_health WHERE waba_id = $1
		ORDER BY phone_number_id NULLS FIRST`, wabaID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ChannelHealth])
}

// ListChannelAccountEvents devuelve el historial reciente. Con onlyProblems se
// omiten los informativos, que es lo que el panel enseña por defecto.
func ListChannelAccountEvents(
	ctx context.Context,
	pool *pgxpool.Pool,
	wabaID string,
	onlyProblems bool,
	limit int,
) ([]ChannelAccountEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := pool.Query(ctx, `
		SELECT id,waba_id,phone_number_id,display_phone,field,severity,occurred_at,payload
		FROM channel_account_events
		WHERE waba_id = $1 AND ($2 = false OR severity <> 'info')
		ORDER BY occurred_at DESC, id DESC LIMIT $3`, wabaID, onlyProblems, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ChannelAccountEvent])
}

// RecentlyLinkedWABAs devuelve las WABA que Meta acaba de vincular a nuestra
// app, de la mas reciente a la mas antigua y sin repetir.
//
// Existe porque el Embedded Signup entrega el waba_id "a la ventana que abrio el
// flujo", y desde el navegador de un movil esa ventana no puede recibirlo. El
// webhook si lo trae, y llega al servidor: PARTNER_ADDED y PARTNER_APP_INSTALLED
// nombran la cuenta recien conectada en payload.waba_info.waba_id, que **no** es
// la columna waba_id —esa identifica a quien emite el evento, y en estos dos
// suele ser el portfolio, no la cuenta vinculada—.
//
// Sirve para desempatar, nunca para autorizar: la lista del token manda, y esto
// solo elige dentro de ella.
func RecentlyLinkedWABAs(ctx context.Context, pool *pgxpool.Pool, window time.Duration) ([]string, error) {
	rows, err := pool.Query(ctx,
		`SELECT payload->'waba_info'->>'waba_id' AS waba
		   FROM channel_account_events
		  WHERE field = 'account_update'
		    AND payload->>'event' IN ('PARTNER_ADDED', 'PARTNER_APP_INSTALLED')
		    AND payload->'waba_info'->>'waba_id' IS NOT NULL
		    AND occurred_at > NOW() - $1::interval
		  ORDER BY occurred_at DESC`,
		window.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var waba string
		if err := rows.Scan(&waba); err != nil {
			return nil, err
		}
		if waba != "" && !seen[waba] {
			seen[waba] = true
			out = append(out, waba)
		}
	}
	return out, rows.Err()
}
