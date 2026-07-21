package models

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Test de integración: requiere DATABASE_URL (se omite si no está).
//   DATABASE_URL=... go test ./models -run OrgMembership -v
func TestOrgMembershipIntegration(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL no seteada; se omite el test de integración")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	u1, u2 := randID("itu1_"), randID("itu2_")
	mustUser(t, ctx, pool, u1, "Owner Test", u1+"@test.local")
	mustUser(t, ctx, pool, u2, "Member Test", u2+"@test.local")
	defer pool.Exec(ctx, `DELETE FROM "user" WHERE id = ANY($1)`, []string{u1, u2})

	// crear org (+ owner membership atómico)
	org, err := CreateOrganization(ctx, pool, u1, "Org de prueba", nil, nil)
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	if org.ID == "" || org.Name != "Org de prueba" {
		t.Fatalf("org inesperada: %+v", org)
	}

	if m, err := GetMembership(ctx, pool, u1, org.ID); err != nil || m == nil || m.Role != "owner" {
		t.Fatalf("owner membership: err=%v m=%+v", err, m)
	}

	if orgs, err := GetUserOrganizations(ctx, pool, u1); err != nil || len(orgs) == 0 {
		t.Fatalf("GetUserOrganizations: err=%v len=%d", err, len(orgs))
	}

	// agregar miembro + duplicado
	mem, err := CreateMembership(ctx, pool, u2, org.ID, "member")
	if err != nil {
		t.Fatalf("CreateMembership: %v", err)
	}
	if _, err := CreateMembership(ctx, pool, u2, org.ID, "member"); !errors.Is(err, ErrDuplicateMember) {
		t.Fatalf("esperaba ErrDuplicateMember, got %v", err)
	}

	// listar miembros (join user)
	views, err := GetOrgMemberships(ctx, pool, org.ID)
	if err != nil || len(views) != 2 {
		t.Fatalf("GetOrgMemberships: err=%v len=%d", err, len(views))
	}
	foundEmail := false
	for _, v := range views {
		if v.Email == u2+"@test.local" {
			foundEmail = true
		}
	}
	if !foundEmail {
		t.Fatal("join a user: email del miembro no encontrado")
	}

	// cambiar rol
	if err := UpdateMembershipRole(ctx, pool, mem.ID, "admin"); err != nil {
		t.Fatalf("UpdateMembershipRole: %v", err)
	}
	if got, _ := GetMembershipByID(ctx, pool, mem.ID); got == nil || got.Role != "admin" {
		t.Fatalf("rol no actualizado: %+v", got)
	}

	// actualizar org (COALESCE ruc)
	ruc := "20123456789"
	uorg, err := UpdateOrganization(ctx, pool, org.ID, "Org renombrada", &ruc, nil)
	if err != nil || uorg == nil || uorg.Name != "Org renombrada" || uorg.RUC == nil || *uorg.RUC != ruc {
		t.Fatalf("UpdateOrganization: err=%v org=%+v", err, uorg)
	}

	// borrar miembro y org (cascade)
	if err := DeleteMembership(ctx, pool, mem.ID); err != nil {
		t.Fatalf("DeleteMembership: %v", err)
	}
	if err := DeleteOrganization(ctx, pool, org.ID); err != nil {
		t.Fatalf("DeleteOrganization: %v", err)
	}
	if gone, _ := GetOrganization(ctx, pool, org.ID); gone != nil {
		t.Fatal("la org no fue eliminada")
	}
}

func randID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func mustUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, name, email string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO "user"(id, name, email, "emailVerified") VALUES ($1, $2, $3, false)`,
		id, name, email); err != nil {
		t.Fatalf("insert user: %v", err)
	}
}
