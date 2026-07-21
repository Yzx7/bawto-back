package models

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ContactField struct {
	ID        string    `db:"id" json:"id"`
	OrgID     string    `db:"org_id" json:"orgId"`
	Key       string    `db:"key" json:"key"`
	Label     string    `db:"label" json:"label"`
	Type      string    `db:"type" json:"type"`
	Required  bool      `db:"required" json:"required"`
	CreatedAt time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt time.Time `db:"updated_at" json:"updatedAt"`
}

type Audience struct {
	ID        string          `db:"id" json:"id"`
	OrgID     string          `db:"org_id" json:"orgId"`
	Name      string          `db:"name" json:"name"`
	Kind      string          `db:"kind" json:"kind"`
	Filter    json.RawMessage `db:"filter" json:"filter"`
	CreatedAt time.Time       `db:"created_at" json:"createdAt"`
	UpdatedAt time.Time       `db:"updated_at" json:"updatedAt"`
}

const fieldCols = `id::text AS id, org_id::text AS org_id, key, label, type, required, created_at, updated_at`
const audienceCols = `id::text AS id, org_id::text AS org_id, name, kind, filter, created_at, updated_at`

func ListContactFields(ctx context.Context, pool *pgxpool.Pool, botID string) ([]ContactField, error) {
	rows, err := pool.Query(ctx, `SELECT `+fieldCols+` FROM contact_fields WHERE org_id=(SELECT org_id FROM bots WHERE id=$1::uuid) ORDER BY created_at`, botID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ContactField])
}

func UpsertContactField(ctx context.Context, pool *pgxpool.Pool, botID, key, label, typ string, required bool) (*ContactField, error) {
	rows, err := pool.Query(ctx, `INSERT INTO contact_fields (org_id, key, label, type, required)
        SELECT org_id,$2,$3,$4,$5 FROM bots WHERE id=$1::uuid
        ON CONFLICT (org_id, key) DO UPDATE SET label = EXCLUDED.label, type = EXCLUDED.type, required = EXCLUDED.required
        RETURNING `+fieldCols, botID, key, label, typ, required)
	if err != nil {
		return nil, err
	}
	v, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[ContactField])
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func ListAudiences(ctx context.Context, pool *pgxpool.Pool, botID string) ([]Audience, error) {
	rows, err := pool.Query(ctx, `SELECT `+audienceCols+` FROM audiences WHERE org_id=(SELECT org_id FROM bots WHERE id=$1::uuid) ORDER BY created_at DESC`, botID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Audience])
}

func CreateAudience(ctx context.Context, pool *pgxpool.Pool, botID, name, kind string, filter json.RawMessage) (*Audience, error) {
	if len(filter) == 0 {
		filter = json.RawMessage(`{}`)
	}
	rows, err := pool.Query(ctx, `INSERT INTO audiences (org_id, name, kind, filter) SELECT org_id,$2,$3,$4::jsonb FROM bots WHERE id=$1::uuid RETURNING `+audienceCols, botID, name, kind, filter)
	if err != nil {
		return nil, err
	}
	v, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Audience])
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func AddAudienceContact(ctx context.Context, pool *pgxpool.Pool, botID, audienceID, contactID string) error {
	cmd, err := pool.Exec(ctx, `INSERT INTO audience_contacts (audience_id, contact_id)
		SELECT a.id, c.id FROM audiences a JOIN contacts c ON c.org_id = a.org_id
		WHERE a.id = $2::uuid AND c.id = $3::uuid AND a.org_id = (SELECT org_id FROM bots WHERE id=$1::uuid)
        ON CONFLICT DO NOTHING`, botID, audienceID, contactID)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		return errors.New("audiencia o contacto no pertenece al bot")
	}
	return nil
}

// ResolveAudience entrega contactos activos. Los filtros dinámicos soportados son
// status, billingStatus, dueBefore y dueAfter; son suficientes para recordatorios.
func ResolveAudience(ctx context.Context, pool *pgxpool.Pool, botID, audienceID string) ([]Contact, error) {
	rows, err := pool.Query(ctx, `SELECT `+audienceCols+` FROM audiences WHERE id = $1::uuid AND org_id = (SELECT org_id FROM bots WHERE id=$2::uuid)`, audienceID, botID)
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Audience])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if a.Kind == "manual" {
		rows, err := pool.Query(ctx, `SELECT `+contactCols+` FROM contacts c JOIN audience_contacts ac ON ac.contact_id = c.id WHERE ac.audience_id = $1::uuid ORDER BY c.created_at`, a.ID)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[Contact])
	}
	var f struct{ Status, BillingStatus, DueBefore, DueAfter string }
	if json.Unmarshal(a.Filter, &f) != nil {
		return nil, errors.New("filtro de audiencia inválido")
	}
	query := `SELECT DISTINCT ` + contactCols + ` FROM contacts c LEFT JOIN billing_records b ON b.contact_id = c.id WHERE c.org_id = (SELECT org_id FROM bots WHERE id=$1::uuid)`
	args := []any{botID}
	if f.Status != "" {
		args = append(args, f.Status)
		query += ` AND c.status = $` + itoa(len(args))
	}
	if f.BillingStatus != "" {
		args = append(args, f.BillingStatus)
		query += ` AND b.status = $` + itoa(len(args))
	}
	if f.DueBefore != "" {
		args = append(args, f.DueBefore)
		query += ` AND b.due_date <= $` + itoa(len(args)) + `::date`
	}
	if f.DueAfter != "" {
		args = append(args, f.DueAfter)
		query += ` AND b.due_date >= $` + itoa(len(args)) + `::date`
	}
	query += ` ORDER BY c.created_at`
	rows, err = pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Contact])
}

func itoa(n int) string { return string(rune('0' + n)) }
