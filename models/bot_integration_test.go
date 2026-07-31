package models

import (
	"context"
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
	if bots, err := ListBotsByOrg(ctx, pool, org.ID); err != nil || len(bots) != 1 {
		t.Fatalf("ListBotsByOrg: err=%v len=%d", err, len(bots))
	}

	// renombrar
	if err := UpdateBotName(ctx, pool, bot.ID, "Bot renombrado"); err != nil {
		t.Fatalf("UpdateBotName: %v", err)
	}

	got, err := GetBot(ctx, pool, bot.ID)
	if err != nil || got == nil || got.Name != "Bot renombrado" {
		t.Fatalf("GetBot: err=%v bot=%+v", err, got)
	}

	// Embedded Signup conserva la relación exacta número → WABA y los
	// metadatos necesarios para templates/pacing.
	channelID := "PNID-" + randID("")
	if err := UpdateBotChannelEmbedded(
		ctx, pool, bot.ID, "wsp", channelID, "51953096022",
		"WABA-1721895455603060", "BUSINESS-1", []byte("encrypted-token"),
	); err != nil {
		t.Fatalf("UpdateBotChannelEmbedded: %v", err)
	}
	got, err = GetBot(ctx, pool, bot.ID)
	if err != nil || got == nil {
		t.Fatalf("GetBot tras Embedded Signup: err=%v bot=%+v", err, got)
	}
	if got.WabaID == nil || *got.WabaID != "WABA-1721895455603060" {
		t.Fatalf("waba_id no persistido: %+v", got.WabaID)
	}
	if got.BusinessID == nil || *got.BusinessID != "BUSINESS-1" {
		t.Fatalf("business_id no persistido: %+v", got.BusinessID)
	}
	if got.ChannelConnectedAt == nil {
		t.Fatal("channel_connected_at no persistido")
	}
	channel, err := GetBotByChannel(ctx, pool, "wsp", channelID)
	if err != nil || channel == nil || channel.WabaID == nil || *channel.WabaID != "WABA-1721895455603060" {
		t.Fatalf("GetBotByChannel sin metadatos: err=%v channel=%+v", err, channel)
	}

	// Una reconexión de la misma WABA sin business_id no debe borrar el que ya
	// se obtuvo; una conexión manual a otro número sí debe limpiar la relación.
	if err := UpdateBotChannelEmbedded(
		ctx, pool, bot.ID, "wsp", channelID, "51953096022",
		"WABA-1721895455603060", "", []byte("rotated-token"),
	); err != nil {
		t.Fatalf("reconectar misma WABA: %v", err)
	}
	got, _ = GetBot(ctx, pool, bot.ID)
	if got.BusinessID == nil || *got.BusinessID != "BUSINESS-1" {
		t.Fatalf("la reconexión borró business_id: %+v", got.BusinessID)
	}
	if err := UpdateBotChannel(ctx, pool, bot.ID, "wsp", channelID+"-NEW", "", []byte("manual-token")); err != nil {
		t.Fatalf("UpdateBotChannel manual: %v", err)
	}
	got, _ = GetBot(ctx, pool, bot.ID)
	if got.WabaID != nil || got.BusinessID != nil {
		t.Fatalf("un número nuevo conservó metadatos de la WABA anterior: waba=%v business=%v", got.WabaID, got.BusinessID)
	}

	// eliminar
	if err := DeleteBot(ctx, pool, bot.ID); err != nil {
		t.Fatalf("DeleteBot: %v", err)
	}
	if gone, _ := GetBot(ctx, pool, bot.ID); gone != nil {
		t.Fatal("el bot no fue eliminado")
	}
}
