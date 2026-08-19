package models

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Organization struct {
	ID             string    `db:"id" json:"id"`
	Name           string    `db:"name" json:"name"`
	RUC            *string   `db:"ruc" json:"ruc,omitempty"`
	Cel            *string   `db:"cel" json:"cel,omitempty"`
	ActivationCode string    `db:"activation_code" json:"activationCode"`
	CreatedBy      *string   `db:"created_by" json:"createdBy,omitempty"`
	CreatedAt      time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt      time.Time `db:"updated_at" json:"updatedAt"`
}

const orgCols = `id::text AS id, name, ruc, cel, activation_code, created_by, created_at, updated_at`

// CreateOrganization crea la org, su membership owner y sus tablas por defecto
// en una transacción.
//
// La semilla va dentro y no después: las tablas y el grafo del bot son una sola
// pieza —cada `object` que el flujo nombra tiene que existir el día que la
// organización nace, porque nadie va a revisarlo antes—, así que una organización
// a la que le falte una tabla es peor que ninguna organización.
func CreateOrganization(ctx context.Context, pool *pgxpool.Pool, userID, name string, ruc, cel *string) (*Organization, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx,
		`INSERT INTO organizations (name, ruc, cel, created_by)
		 VALUES ($1, $2, $3, $4) RETURNING `+orgCols,
		name, ruc, cel, userID)
	if err != nil {
		return nil, err
	}
	org, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Organization])
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO memberships (user_id, org_id, role) VALUES ($1, $2::uuid, 'owner')`,
		userID, org.ID); err != nil {
		return nil, err
	}
	if err := seedOrganizationTx(ctx, tx, org.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &org, nil
}

// GetUserOrganizations devuelve las orgs donde el usuario es miembro.
func GetUserOrganizations(ctx context.Context, pool *pgxpool.Pool, userID string) ([]Organization, error) {
	rows, err := pool.Query(ctx,
		`SELECT o.id::text AS id, o.name, o.ruc, o.cel, o.activation_code, o.created_by, o.created_at, o.updated_at
		 FROM organizations o
		 JOIN memberships m ON m.org_id = o.id
		 WHERE m.user_id = $1
		 ORDER BY o.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Organization])
}

// GetOrganization devuelve una org por id (nil si no existe).
func GetOrganization(ctx context.Context, pool *pgxpool.Pool, orgID string) (*Organization, error) {
	rows, err := pool.Query(ctx, `SELECT `+orgCols+` FROM organizations WHERE id = $1::uuid`, orgID)
	if err != nil {
		return nil, err
	}
	org, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Organization])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &org, nil
}

// UpdateOrganization actualiza nombre siempre; ruc/cel solo si vienen (COALESCE).
func UpdateOrganization(ctx context.Context, pool *pgxpool.Pool, orgID, name string, ruc, cel *string) (*Organization, error) {
	rows, err := pool.Query(ctx,
		`UPDATE organizations
		 SET name = $2, ruc = COALESCE($3, ruc), cel = COALESCE($4, cel)
		 WHERE id = $1::uuid
		 RETURNING `+orgCols, orgID, name, ruc, cel)
	if err != nil {
		return nil, err
	}
	org, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Organization])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &org, nil
}

// DeleteOrganization elimina la org (memberships/bots caen por FK cascade).
func DeleteOrganization(ctx context.Context, pool *pgxpool.Pool, orgID string) error {
	_, err := pool.Exec(ctx, `DELETE FROM organizations WHERE id = $1::uuid`, orgID)
	return err
}
