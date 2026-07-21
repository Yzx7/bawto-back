package models

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Membership struct {
	ID        string    `db:"id" json:"id"`
	UserID    string    `db:"user_id" json:"userId"`
	OrgID     string    `db:"org_id" json:"orgId"`
	Role      string    `db:"role" json:"role"`
	CreatedAt time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt time.Time `db:"updated_at" json:"updatedAt"`
}

// MembershipView incluye datos del usuario para listar miembros.
type MembershipView struct {
	ID     string `db:"id" json:"id"`
	UserID string `db:"user_id" json:"userId"`
	OrgID  string `db:"org_id" json:"orgId"`
	Role   string `db:"role" json:"role"`
	Name   string `db:"name" json:"name"`
	Email  string `db:"email" json:"email"`
}

// ErrDuplicateMember indica que el usuario ya es miembro de la org.
var ErrDuplicateMember = errors.New("el usuario ya es miembro")

const memCols = `id::text AS id, user_id, org_id::text AS org_id, role, created_at, updated_at`

// GetMembership devuelve el membership de un usuario en una org (nil si no hay).
func GetMembership(ctx context.Context, pool *pgxpool.Pool, userID, orgID string) (*Membership, error) {
	return collectMembership(ctx, pool,
		`SELECT `+memCols+` FROM memberships WHERE user_id = $1 AND org_id = $2::uuid`,
		userID, orgID)
}

// GetMembershipByID devuelve un membership por id (nil si no existe).
func GetMembershipByID(ctx context.Context, pool *pgxpool.Pool, id string) (*Membership, error) {
	return collectMembership(ctx, pool,
		`SELECT `+memCols+` FROM memberships WHERE id = $1::uuid`, id)
}

func collectMembership(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) (*Membership, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	m, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Membership])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// GetOrgMemberships lista los miembros de una org con nombre/email del usuario.
func GetOrgMemberships(ctx context.Context, pool *pgxpool.Pool, orgID string) ([]MembershipView, error) {
	rows, err := pool.Query(ctx,
		`SELECT m.id::text AS id, m.user_id, m.org_id::text AS org_id, m.role, u.name, u.email
		 FROM memberships m
		 JOIN "user" u ON u.id = m.user_id
		 WHERE m.org_id = $1::uuid
		 ORDER BY m.created_at`, orgID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[MembershipView])
}

// FindUserIDByEmail busca el id de un usuario de Better Auth por email ("" si no existe).
func FindUserIDByEmail(ctx context.Context, pool *pgxpool.Pool, email string) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `SELECT id FROM "user" WHERE email = $1`, email).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// CreateMembership agrega un miembro; ErrDuplicateMember si ya pertenece.
func CreateMembership(ctx context.Context, pool *pgxpool.Pool, userID, orgID, role string) (*Membership, error) {
	rows, err := pool.Query(ctx,
		`INSERT INTO memberships (user_id, org_id, role) VALUES ($1, $2::uuid, $3)
		 ON CONFLICT (org_id, user_id) DO NOTHING RETURNING `+memCols,
		userID, orgID, role)
	if err != nil {
		return nil, err
	}
	m, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Membership])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDuplicateMember
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// UpdateMembershipRole cambia el rol de un membership.
func UpdateMembershipRole(ctx context.Context, pool *pgxpool.Pool, id, role string) error {
	_, err := pool.Exec(ctx, `UPDATE memberships SET role = $2 WHERE id = $1::uuid`, id, role)
	return err
}

// DeleteMembership elimina un membership.
func DeleteMembership(ctx context.Context, pool *pgxpool.Pool, id string) error {
	_, err := pool.Exec(ctx, `DELETE FROM memberships WHERE id = $1::uuid`, id)
	return err
}
