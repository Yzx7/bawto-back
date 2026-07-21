package models

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBotIntegration(t *testing.T) {
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

	u := randID("bitu_")
	mustUser(t, ctx, pool, u, "Bot Owner", u+"@test.local")
	defer pool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, u)

	org, err := CreateOrganization(ctx, pool, u, "Org bots", nil, nil)
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	defer DeleteOrganization(ctx, pool, org.ID)

	// crear bot (sin canal)
	bot, err := CreateBot(ctx, pool, org.ID, "Bot 1", "")
	if err != nil || bot.Name != "Bot 1" || bot.Channel != "wsp" {
		t.Fatalf("CreateBot: err=%v bot=%+v", err, bot)
	}
	if string(bot.Flow) != "{}" {
		t.Fatalf("flow por defecto esperado {}, got %q", string(bot.Flow))
	}

	if bots, err := ListBotsByOrg(ctx, pool, org.ID); err != nil || len(bots) != 1 {
		t.Fatalf("ListBotsByOrg: err=%v len=%d", err, len(bots))
	}

	// renombrar + guardar flujo
	if err := UpdateBotName(ctx, pool, bot.ID, "Bot renombrado"); err != nil {
		t.Fatalf("UpdateBotName: %v", err)
	}
	flow := json.RawMessage(`{"id":"f1","name":"F","trigger":{"type":"message","match":"any"},"nodes":[],"edges":[]}`)
	if err := UpdateBotFlow(ctx, pool, bot.ID, flow); err != nil {
		t.Fatalf("UpdateBotFlow: %v", err)
	}

	got, err := GetBot(ctx, pool, bot.ID)
	if err != nil || got == nil || got.Name != "Bot renombrado" {
		t.Fatalf("GetBot: err=%v bot=%+v", err, got)
	}
	var parsed map[string]any
	if err := json.Unmarshal(got.Flow, &parsed); err != nil || parsed["name"] != "F" {
		t.Fatalf("flow guardado mal: err=%v parsed=%v", err, parsed)
	}

	// eliminar
	if err := DeleteBot(ctx, pool, bot.ID); err != nil {
		t.Fatalf("DeleteBot: %v", err)
	}
	if gone, _ := GetBot(ctx, pool, bot.ID); gone != nil {
		t.Fatal("el bot no fue eliminado")
	}
}
