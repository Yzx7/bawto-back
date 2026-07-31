package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ProviderStatusEventInput struct {
	Channel           string
	ChannelID         string
	ProviderMessageID string
	Status            string
	OccurredAt        time.Time
	RecipientID       string
	ErrorCode         string
	ErrorTitle        string
	ErrorMessage      string
	ErrorDetails      string
	ConversationID    string
	ConversationType  string
	PricingModel      string
	PricingType       string
	PricingCategory   string
	Billable          *bool
	OpaqueCallback    string
	Metadata          json.RawMessage
}

type ProviderStatusUpdate struct {
	MessageID  int64     `json:"messageId"`
	ChatID     string    `json:"chatId"`
	BotID      string    `json:"botId"`
	FlowRunID  *string   `json:"flowRunId,omitempty"`
	Status     string    `json:"status"`
	ErrorCode  string    `json:"errorCode,omitempty"`
	Error      string    `json:"error,omitempty"`
	OccurredAt time.Time `json:"occurredAt"`
}

func ProviderStatusEventKey(event ProviderStatusEventInput) string {
	raw := event.Channel + "\x00" + event.ChannelID + "\x00" + event.ProviderMessageID +
		"\x00" + event.Status + "\x00" + event.OccurredAt.UTC().Format(time.RFC3339Nano) +
		"\x00" + event.ErrorCode
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func StoreProviderStatusEvent(ctx context.Context, p *pgxpool.Pool, event ProviderStatusEventInput) (bool, error) {
	if len(event.Metadata) == 0 {
		event.Metadata = json.RawMessage(`{}`)
	}
	tag, err := p.Exec(ctx, `INSERT INTO provider_status_events
		(event_key,channel,channel_id,provider_message_id,status,occurred_at,recipient_id,
		 error_code,error_title,error_message,error_details,conversation_id,conversation_type,
		 pricing_model,pricing_type,pricing_category,billable,opaque_callback_data,metadata)
		VALUES($1,$2,NULLIF($3,''),$4,$5,$6,NULLIF($7,''),NULLIF($8,''),NULLIF($9,''),
		 NULLIF($10,''),NULLIF($11,''),NULLIF($12,''),NULLIF($13,''),NULLIF($14,''),
		 NULLIF($15,''),NULLIF($16,''),$17,NULLIF($18,''),$19::jsonb)
		ON CONFLICT(event_key) DO NOTHING`,
		ProviderStatusEventKey(event), event.Channel, event.ChannelID, event.ProviderMessageID,
		event.Status, event.OccurredAt.UTC(), event.RecipientID, event.ErrorCode,
		event.ErrorTitle, event.ErrorMessage, event.ErrorDetails, event.ConversationID,
		event.ConversationType, event.PricingModel, event.PricingType, event.PricingCategory,
		event.Billable, event.OpaqueCallback, event.Metadata)
	return tag.RowsAffected() == 1, err
}

type storedProviderStatus struct {
	ID               int64
	Status           string
	OccurredAt       time.Time
	ErrorCode        *string
	ErrorTitle       *string
	ErrorMessage     *string
	ErrorDetails     *string
	ConversationID   *string
	ConversationType *string
	PricingModel     *string
	PricingType      *string
	PricingCategory  *string
	Billable         *bool
}

func statusRank(status string) int {
	switch status {
	case "sent":
		return 1
	case "delivered":
		return 2
	case "read":
		return 3
	case "played":
		return 4
	default:
		return 0
	}
}

func advancedProviderStatus(current, incoming string) (string, bool) {
	if current == "failed" {
		return current, false
	}
	if incoming == "failed" {
		if statusRank(current) < 2 {
			return "failed", true
		}
		return current, false
	}
	if statusRank(incoming) > statusRank(current) {
		return incoming, true
	}
	return current, false
}

// ReconcileProviderStatusEvents aplica en orden los eventos que ya pueden
// relacionarse con messages. Los eventos adelantados permanecen sin applied_at.
func ReconcileProviderStatusEvents(ctx context.Context, p *pgxpool.Pool, providerMessageID string) ([]ProviderStatusUpdate, error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var messageID int64
	var chatID, botID, messageStatus string
	err = tx.QueryRow(ctx, `SELECT m.id,c.id::text,c.bot_id::text,COALESCE(m.provider_status,'')
		FROM messages m JOIN chats c ON c.id=m.chat_id
		WHERE m.wa_id=$1 FOR UPDATE OF m`, providerMessageID).
		Scan(&messageID, &chatID, &botID, &messageStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var runID *string
	var runIDValue string
	var runStatus string
	err = tx.QueryRow(ctx, `SELECT id::text,status FROM flow_runs
		WHERE provider_message_id=$1 FOR UPDATE`, providerMessageID).Scan(&runIDValue, &runStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		runID = nil
		runStatus = ""
	} else if err != nil {
		return nil, err
	} else {
		runID = &runIDValue
	}

	rows, err := tx.Query(ctx, `SELECT id,status,occurred_at,error_code,error_title,error_message,
		error_details,conversation_id,conversation_type,pricing_model,pricing_type,
		pricing_category,billable
		FROM provider_status_events WHERE provider_message_id=$1 AND applied_at IS NULL
		ORDER BY occurred_at,id FOR UPDATE`, providerMessageID)
	if err != nil {
		return nil, err
	}
	var events []storedProviderStatus
	for rows.Next() {
		var event storedProviderStatus
		if err := rows.Scan(&event.ID, &event.Status, &event.OccurredAt, &event.ErrorCode,
			&event.ErrorTitle, &event.ErrorMessage, &event.ErrorDetails, &event.ConversationID,
			&event.ConversationType, &event.PricingModel, &event.PricingType,
			&event.PricingCategory, &event.Billable); err != nil {
			rows.Close()
			return nil, err
		}
		events = append(events, event)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	updates := make([]ProviderStatusUpdate, 0, len(events))
	for _, event := range events {
		nextMessageStatus, messageAdvanced := advancedProviderStatus(messageStatus, event.Status)
		errorText := firstNonEmpty(event.ErrorDetails, event.ErrorMessage, event.ErrorTitle)
		if _, err := tx.Exec(ctx, `UPDATE messages SET
			provider_status=CASE WHEN $3 THEN $2 ELSE provider_status END,
			provider_status_at=CASE WHEN $3 THEN $4 ELSE provider_status_at END,
			provider_error_code=CASE WHEN $2='failed' AND $3 THEN $5 ELSE provider_error_code END,
			provider_error=CASE WHEN $2='failed' AND $3 THEN $6 ELSE provider_error END,
			conversation_id=COALESCE($7,conversation_id),
			conversation_type=COALESCE($8,conversation_type),
			pricing_model=COALESCE($9,pricing_model),
			pricing_type=COALESCE($10,pricing_type),
			pricing_category=COALESCE($11,pricing_category),
			billable=COALESCE($12,billable)
			WHERE id=$1`, messageID, nextMessageStatus, messageAdvanced, event.OccurredAt,
			event.ErrorCode, nullString(errorText), event.ConversationID, event.ConversationType,
			event.PricingModel, event.PricingType, event.PricingCategory, event.Billable); err != nil {
			return nil, err
		}
		if messageAdvanced {
			messageStatus = nextMessageStatus
		}

		runAdvanced := false
		if runID != nil {
			nextRunStatus, advance := advancedProviderStatus(runStatus, event.Status)
			runAdvanced = advance
			if _, err := tx.Exec(ctx, `UPDATE flow_runs SET
				status=CASE WHEN $3 THEN $2 ELSE status END,
				provider_status_at=CASE WHEN $3 THEN $4 ELSE provider_status_at END,
				delivered_at=CASE WHEN $3 AND $2='delivered' THEN $4 ELSE delivered_at END,
				read_at=CASE WHEN $3 AND $2='read' THEN $4 ELSE read_at END,
				played_at=CASE WHEN $3 AND $2='played' THEN $4 ELSE played_at END,
				finished_at=CASE WHEN $3 AND $2='failed' THEN NOW() ELSE finished_at END,
				last_error_code=CASE WHEN $3 AND $2='failed' THEN $5 ELSE last_error_code END,
				last_error_class=CASE WHEN $3 AND $2='failed' THEN 'provider_permanent' ELSE last_error_class END,
				last_error=CASE WHEN $3 AND $2='failed' THEN $6 ELSE last_error END,
				conversation_id=COALESCE($7,conversation_id),
				conversation_type=COALESCE($8,conversation_type),
				pricing_model=COALESCE($9,pricing_model),
				pricing_type=COALESCE($10,pricing_type),
				pricing_category=COALESCE($11,pricing_category),
				billable=COALESCE($12,billable)
				WHERE id=$1::uuid`, *runID, nextRunStatus, advance, event.OccurredAt,
				event.ErrorCode, nullString(errorText), event.ConversationID, event.ConversationType,
				event.PricingModel, event.PricingType, event.PricingCategory, event.Billable); err != nil {
				return nil, err
			}
			if advance {
				runStatus = nextRunStatus
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE provider_status_events SET applied_at=NOW() WHERE id=$1`, event.ID); err != nil {
			return nil, err
		}
		if messageAdvanced || runAdvanced {
			update := ProviderStatusUpdate{
				MessageID: messageID, ChatID: chatID, BotID: botID, FlowRunID: runID,
				Status: messageStatus, OccurredAt: event.OccurredAt,
			}
			if event.ErrorCode != nil {
				update.ErrorCode = *event.ErrorCode
			}
			update.Error = errorText
			updates = append(updates, update)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return updates, nil
}

func firstNonEmpty(values ...*string) string {
	for _, value := range values {
		if value != nil && *value != "" {
			return *value
		}
	}
	return ""
}

func nullString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
