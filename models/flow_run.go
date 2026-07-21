package models

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type FlowRun struct {
	ID           string          `db:"id" json:"id"`
	BotID        string          `db:"bot_id" json:"botId"`
	FlowID       string          `db:"flow_id" json:"flowId"`
	DataRecordID string          `db:"data_record_id" json:"dataRecordId"`
	ContactID    string          `db:"contact_id" json:"contactId"`
	RunKey       string          `db:"run_key" json:"runKey"`
	Status       string          `db:"status" json:"status"`
	Context      json.RawMessage `db:"context" json:"context"`
	Error        *string         `db:"error" json:"error,omitempty"`
	CreatedAt    time.Time       `db:"created_at" json:"createdAt"`
	StartedAt    *time.Time      `db:"started_at" json:"startedAt,omitempty"`
	FinishedAt   *time.Time      `db:"finished_at" json:"finishedAt,omitempty"`
}

const flowRunCols = `id::text AS id, bot_id::text AS bot_id, flow_id, data_record_id::text AS data_record_id, contact_id::text AS contact_id, run_key, status, context, error, created_at, started_at, finished_at`

// CreateFlowRun reserva el run; false significa que ya se procesó esa clave.
func CreateFlowRun(ctx context.Context, p *pgxpool.Pool, botID, flowID, recordID, contactID, key string, flowContext json.RawMessage) (*FlowRun, bool, error) {
	if len(flowContext) == 0 {
		flowContext = json.RawMessage(`{}`)
	}
	var run FlowRun
	err := p.QueryRow(ctx, `INSERT INTO flow_runs (bot_id, flow_id, data_record_id, contact_id, run_key, context)
        VALUES ($1::uuid, $2, $3::uuid, $4::uuid, $5, $6::jsonb)
        ON CONFLICT (bot_id, run_key) DO NOTHING RETURNING `+flowRunCols,
		botID, flowID, recordID, contactID, key, flowContext).Scan(&run.ID, &run.BotID, &run.FlowID, &run.DataRecordID, &run.ContactID, &run.RunKey, &run.Status, &run.Context, &run.Error, &run.CreatedAt, &run.StartedAt, &run.FinishedAt)
	if err != nil {
		// pgx.ErrNoRows es el resultado esperado de DO NOTHING.
		if err == pgx.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &run, true, nil
}

func ClaimFlowRun(ctx context.Context, p *pgxpool.Pool) (*FlowRun, error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var run FlowRun
	err = tx.QueryRow(ctx, `SELECT `+flowRunCols+` FROM flow_runs WHERE status='queued' ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&run.ID, &run.BotID, &run.FlowID, &run.DataRecordID, &run.ContactID, &run.RunKey, &run.Status, &run.Context, &run.Error, &run.CreatedAt, &run.StartedAt, &run.FinishedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE flow_runs SET status='running',started_at=NOW(),error=NULL WHERE id=$1::uuid`, run.ID); err != nil {
		return nil, err
	}
	run.Status = "running"
	now := time.Now()
	run.StartedAt = &now
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &run, nil
}
func FinishFlowRun(ctx context.Context, p *pgxpool.Pool, id string) error {
	_, e := p.Exec(ctx, `UPDATE flow_runs SET status='sent',finished_at=NOW(),error=NULL WHERE id=$1::uuid`, id)
	return e
}
func FailFlowRun(ctx context.Context, p *pgxpool.Pool, id string, cause error) error {
	_, e := p.Exec(ctx, `UPDATE flow_runs SET status='failed',finished_at=NOW(),error=$2 WHERE id=$1::uuid`, id, cause.Error())
	return e
}
