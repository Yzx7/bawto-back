package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ScheduledFlow struct {
	FlowID        string          `db:"flow_id"`
	BotID         string          `db:"bot_id"`
	FlowVersionID string          `db:"flow_version_id"`
	Definition    json.RawMessage `db:"definition"`
	LastTickAt    *time.Time      `db:"last_tick_at"`
}

type FlowRun struct {
	ID                string          `db:"id" json:"id"`
	BotID             string          `db:"bot_id" json:"botId"`
	FlowID            string          `db:"flow_id" json:"flowId"`
	FlowVersionID     string          `db:"flow_version_id" json:"flowVersionId"`
	DataRecordID      *string         `db:"data_record_id" json:"dataRecordId,omitempty"`
	ContactID         *string         `db:"contact_id" json:"contactId,omitempty"`
	RunKey            string          `db:"run_key" json:"runKey"`
	Status            string          `db:"status" json:"status"`
	ScheduledFor      time.Time       `db:"scheduled_for" json:"scheduledFor"`
	Source            string          `db:"source" json:"source"`
	Attempt           int             `db:"attempt" json:"attempt"`
	MaxAttempts       int             `db:"max_attempts" json:"maxAttempts"`
	PostponementCount int             `db:"postponement_count" json:"postponementCount"`
	NextAttemptAt     time.Time       `db:"next_attempt_at" json:"nextAttemptAt"`
	ProviderMessageID *string         `db:"provider_message_id" json:"providerMessageId,omitempty"`
	ProviderStatusAt  *time.Time      `db:"provider_status_at" json:"providerStatusAt,omitempty"`
	DeliveredAt       *time.Time      `db:"delivered_at" json:"deliveredAt,omitempty"`
	ReadAt            *time.Time      `db:"read_at" json:"readAt,omitempty"`
	PlayedAt          *time.Time      `db:"played_at" json:"playedAt,omitempty"`
	ConversationID    *string         `db:"conversation_id" json:"conversationId,omitempty"`
	ConversationType  *string         `db:"conversation_type" json:"conversationType,omitempty"`
	PricingModel      *string         `db:"pricing_model" json:"pricingModel,omitempty"`
	PricingType       *string         `db:"pricing_type" json:"pricingType,omitempty"`
	PricingCategory   *string         `db:"pricing_category" json:"pricingCategory,omitempty"`
	Billable          *bool           `db:"billable" json:"billable,omitempty"`
	Context           json.RawMessage `db:"context" json:"context"`
	LastErrorCode     *string         `db:"last_error_code" json:"lastErrorCode,omitempty"`
	LastErrorClass    *string         `db:"last_error_class" json:"lastErrorClass,omitempty"`
	LastError         *string         `db:"last_error" json:"lastError,omitempty"`
	CreatedAt         time.Time       `db:"created_at" json:"createdAt"`
	StartedAt         *time.Time      `db:"started_at" json:"startedAt,omitempty"`
	FinishedAt        *time.Time      `db:"finished_at" json:"finishedAt,omitempty"`
}

const flowRunCols = `id::text AS id, bot_id::text AS bot_id, flow_id::text AS flow_id,
	flow_version_id::text AS flow_version_id, data_record_id::text AS data_record_id,
	contact_id::text AS contact_id, run_key, status, scheduled_for, source, attempt,
	max_attempts, postponement_count, next_attempt_at, provider_message_id, context,
	provider_status_at,delivered_at,read_at,played_at,conversation_id,conversation_type,
	pricing_model,pricing_type,pricing_category,billable,last_error_code,last_error_class,last_error,
	created_at,started_at,finished_at`

func ListPublishedScheduleFlows(ctx context.Context, p *pgxpool.Pool) ([]ScheduledFlow, error) {
	rows, err := p.Query(ctx, `SELECT f.id::text AS flow_id, f.bot_id::text AS bot_id,
		v.id::text AS flow_version_id, v.definition, f.last_tick_at
		FROM flows f JOIN flow_versions v ON v.id=f.published_version_id
		WHERE f.trigger_type='schedule' AND f.status='published' AND f.archived_at IS NULL
		ORDER BY f.id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ScheduledFlow])
}

func AdvanceFlowTick(ctx context.Context, p *pgxpool.Pool, flowID string, at time.Time) error {
	_, err := p.Exec(ctx, `UPDATE flows SET last_tick_at=$2 WHERE id=$1::uuid`, flowID, at.UTC())
	return err
}

func RecordScheduleOccurrence(ctx context.Context, p *pgxpool.Pool, flowID, versionID string, at time.Time, status, reason string, queued, skipped int) error {
	_, err := p.Exec(ctx, `INSERT INTO flow_schedule_occurrences
		(flow_id,flow_version_id,scheduled_for,status,reason,queued_count,skipped_count)
		VALUES($1::uuid,$2::uuid,$3,$4,NULLIF($5,''),$6,$7)
		ON CONFLICT(flow_id,scheduled_for) DO UPDATE SET status=EXCLUDED.status,
		reason=EXCLUDED.reason,queued_count=EXCLUDED.queued_count,skipped_count=EXCLUDED.skipped_count`,
		flowID, versionID, at.UTC(), status, reason, queued, skipped)
	return err
}

func CreateFlowRun(ctx context.Context, p *pgxpool.Pool, botID, flowID, versionID, recordID, contactID, key string, scheduledFor time.Time, flowContext json.RawMessage) (*FlowRun, bool, error) {
	if len(flowContext) == 0 {
		flowContext = json.RawMessage(`{}`)
	}
	rows, err := p.Query(ctx, `INSERT INTO flow_runs
		(bot_id,flow_id,flow_version_id,data_record_id,contact_id,run_key,scheduled_for,context)
		VALUES($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,$6,$7,$8::jsonb)
		ON CONFLICT(run_key) DO NOTHING RETURNING `+flowRunCols,
		botID, flowID, versionID, recordID, contactID, key, scheduledFor.UTC(), flowContext)
	if err != nil {
		return nil, false, err
	}
	run, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowRun])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &run, true, nil
}

func RecoverStaleFlowRuns(ctx context.Context, p *pgxpool.Pool, staleBefore time.Time) (int64, error) {
	tag, err := p.Exec(ctx, `UPDATE flow_runs SET status='retry_wait',next_attempt_at=NOW(),
		locked_at=NULL,locked_by=NULL,heartbeat_at=NULL,last_error_class='worker_lost',
		last_error='worker perdió el lease'
		WHERE status='running' AND COALESCE(heartbeat_at,locked_at,started_at)<$1`, staleBefore)
	return tag.RowsAffected(), err
}

func ClaimFlowRun(ctx context.Context, p *pgxpool.Pool, workerID string) (*FlowRun, error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `SELECT id::text FROM flow_runs
		WHERE status IN ('queued','retry_wait') AND next_attempt_at<=NOW()
		ORDER BY next_attempt_at,created_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `UPDATE flow_runs SET status='running',attempt=attempt+1,
		started_at=COALESCE(started_at,NOW()),locked_at=NOW(),locked_by=$2,heartbeat_at=NOW(),
		last_error=NULL,last_error_code=NULL,last_error_class=NULL
		WHERE id=$1::uuid RETURNING `+flowRunCols, id, workerID)
	if err != nil {
		return nil, err
	}
	run, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowRun])
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &run, nil
}

func RunnableFlowDefinition(ctx context.Context, p *pgxpool.Pool, run *FlowRun) (json.RawMessage, error) {
	var raw json.RawMessage
	err := p.QueryRow(ctx, `SELECT v.definition FROM flows f JOIN flow_versions v ON v.id=$2::uuid AND v.flow_id=f.id
		WHERE f.id=$1::uuid AND f.status='published' AND f.archived_at IS NULL`, run.FlowID, run.FlowVersionID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return raw, err
}

func FinishFlowRun(ctx context.Context, p *pgxpool.Pool, id, providerID string) error {
	_, err := p.Exec(ctx, `UPDATE flow_runs SET
		status=CASE WHEN status='running' THEN 'sent' ELSE status END,finished_at=NOW(),
		provider_message_id=COALESCE(provider_message_id,NULLIF($2,'')),
		locked_at=NULL,locked_by=NULL,heartbeat_at=NULL
		WHERE id=$1::uuid`, id, providerID)
	return err
}

// AttachFlowRunProviderMessage se ejecuta inmediatamente tras la aceptación de
// Meta, antes de cualquier escritura auxiliar. Así un status adelantado ya puede
// encontrar el run y nunca obliga a reenviar.
func AttachFlowRunProviderMessage(ctx context.Context, p *pgxpool.Pool, id, providerID string) error {
	_, err := p.Exec(ctx, `UPDATE flow_runs SET provider_message_id=$2,status='sent',
		finished_at=NOW() WHERE id=$1::uuid AND status='running'`, id, providerID)
	return err
}

func RetryFlowRun(ctx context.Context, p *pgxpool.Pool, id, code, class, message string, next time.Time) error {
	_, err := p.Exec(ctx, `UPDATE flow_runs SET status=CASE WHEN attempt>=max_attempts THEN 'dead' ELSE 'retry_wait' END,
		next_attempt_at=$2,last_error_code=NULLIF($3,''),last_error_class=$4,last_error=$5,
		finished_at=CASE WHEN attempt>=max_attempts THEN NOW() ELSE NULL END,
		locked_at=NULL,locked_by=NULL,heartbeat_at=NULL WHERE id=$1::uuid`, id, next, code, class, message)
	return err
}

func FailFlowRun(ctx context.Context, p *pgxpool.Pool, id, code, class, message string) error {
	_, err := p.Exec(ctx, `UPDATE flow_runs SET status='failed',finished_at=NOW(),
		last_error_code=NULLIF($2,''),last_error_class=$3,last_error=$4,
		locked_at=NULL,locked_by=NULL,heartbeat_at=NULL WHERE id=$1::uuid`, id, code, class, message)
	return err
}

func UnverifyFlowRun(ctx context.Context, p *pgxpool.Pool, id, message string) error {
	_, err := p.Exec(ctx, `UPDATE flow_runs SET status='unverified',finished_at=NOW(),
		last_error_class='ambiguous',last_error=$2,locked_at=NULL,locked_by=NULL,heartbeat_at=NULL
		WHERE id=$1::uuid`, id, message)
	return err
}

func CancelFlowRun(ctx context.Context, p *pgxpool.Pool, id, reason string) error {
	_, err := p.Exec(ctx, `UPDATE flow_runs SET status='cancelled',cancel_reason=$2,finished_at=NOW(),
		locked_at=NULL,locked_by=NULL,heartbeat_at=NULL WHERE id=$1::uuid`, id, reason)
	return err
}

func PostponeFlowRun(ctx context.Context, p *pgxpool.Pool, id, reason string, next time.Time, max int) error {
	tag, err := p.Exec(ctx, `UPDATE flow_runs SET
		status=CASE WHEN postponement_count+1 >= $4 THEN 'cancelled' ELSE 'retry_wait' END,
		postponement_count=postponement_count+1,attempt=GREATEST(attempt-1,0),
		next_attempt_at=$3,cancel_reason=CASE WHEN postponement_count+1 >= $4 THEN $2 ELSE NULL END,
		finished_at=CASE WHEN postponement_count+1 >= $4 THEN NOW() ELSE NULL END,
		last_error_class='postponed',last_error=$2,locked_at=NULL,locked_by=NULL,heartbeat_at=NULL
		WHERE id=$1::uuid`, id, reason, next, max)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("flow run no encontrado")
	}
	return nil
}

func RecordStillInView(ctx context.Context, p *pgxpool.Pool, botID, viewID, recordID string) (bool, error) {
	records, err := ResolveDataView(ctx, p, botID, viewID)
	if err != nil {
		return false, err
	}
	for _, record := range records {
		if record.ID == recordID {
			return true, nil
		}
	}
	return false, nil
}

func RecordStillInViewAt(
	ctx context.Context,
	p *pgxpool.Pool,
	botID, viewID, recordID string,
	reference time.Time,
) (bool, error) {
	records, err := ResolveDataViewAt(ctx, p, botID, viewID, reference)
	if err != nil {
		return false, err
	}
	for _, record := range records {
		if record.ID == recordID {
			return true, nil
		}
	}
	return false, nil
}

func ReminderCapReached(ctx context.Context, p *pgxpool.Pool, flowID, recordID string, cap int) (bool, error) {
	var n int
	err := p.QueryRow(ctx, `SELECT COUNT(*)::int FROM flow_runs
		WHERE flow_id=$1::uuid AND data_record_id=$2::uuid
		  AND status IN ('sent','delivered','read','played')`, flowID, recordID).Scan(&n)
	return n >= cap, err
}

func ReminderChatBlock(ctx context.Context, p *pgxpool.Pool, botID, phone string) (string, error) {
	var mode string
	var until *time.Time
	var state json.RawMessage
	err := p.QueryRow(ctx, `SELECT mode,handoff_until,current_layer FROM chats
		WHERE bot_id=$1::uuid AND contact=$2`, botID, NormalizePhone(phone)).Scan(&mode, &until, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if mode == "manual" && (until == nil || until.After(time.Now())) {
		return "chat en atención manual", nil
	}
	if mode == "manual" && until != nil && until.Before(time.Now()) {
		_, _ = p.Exec(ctx, `UPDATE chats SET mode='bot',handoff_until=NULL WHERE bot_id=$1::uuid AND contact=$2`, botID, NormalizePhone(phone))
	}
	var value any
	if json.Unmarshal(state, &value) == nil && value != nil {
		switch v := value.(type) {
		case []any:
			if len(v) > 0 {
				return "chat con flujo conversacional activo", nil
			}
		case map[string]any:
			if len(v) > 0 {
				return "chat con flujo conversacional activo", nil
			}
		}
	}
	return "", nil
}

func ReserveWABASlot(ctx context.Context, p *pgxpool.Pool, wabaID string, spacing time.Duration) (time.Duration, error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO waba_delivery_state(waba_id) VALUES($1) ON CONFLICT DO NOTHING`, wabaID); err != nil {
		return 0, err
	}
	var slot time.Time
	err = tx.QueryRow(ctx, `SELECT GREATEST(NOW(),next_allowed_at,COALESCE(paused_until,NOW()))
		FROM waba_delivery_state WHERE waba_id=$1 FOR UPDATE`, wabaID).Scan(&slot)
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `UPDATE waba_delivery_state SET next_allowed_at=$2,updated_at=NOW() WHERE waba_id=$1`,
		wabaID, slot.Add(spacing)); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	wait := time.Until(slot)
	if wait < 0 {
		wait = 0
	}
	return wait, nil
}

func PauseWABA(ctx context.Context, p *pgxpool.Pool, wabaID string, until time.Time) error {
	_, err := p.Exec(ctx, `INSERT INTO waba_delivery_state(waba_id,paused_until)
		VALUES($1,$2) ON CONFLICT(waba_id) DO UPDATE SET paused_until=GREATEST(COALESCE(waba_delivery_state.paused_until,NOW()),$2),updated_at=NOW()`,
		wabaID, until)
	return err
}
