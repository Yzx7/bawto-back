package models

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Yzx7/sacs-chatbots/channels/whatsapp"
)

func prefEvento(waID, category, value string, at time.Time) whatsapp.UserPreference {
	return whatsapp.UserPreference{
		WaID: waID, Category: category, Value: value, OccurredAt: at,
		EventKey: randID("pref_"), Payload: json.RawMessage(`{"value":"` + value + `"}`),
	}
}

func TestPreferenciaDeMarketing(t *testing.T) {
	pool := accountPool(t)
	defer pool.Close()
	ctx := context.Background()

	uid := randID("prefu_")
	if _, err := pool.Exec(ctx, `INSERT INTO "user"(id,name,email,"emailVerified") VALUES ($1,$2,$3,false)`,
		uid, "Pref Owner", uid+"@test.local"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, uid)

	org, err := CreateOrganization(ctx, pool, uid, "Pref Org", nil, nil)
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	defer DeleteOrganization(ctx, pool, org.ID)

	bot, err := CreateBot(ctx, pool, org.ID, "Pref Bot", "wsp")
	if err != nil {
		t.Fatalf("CreateBot: %v", err)
	}
	contact, err := EnsureInboundContact(ctx, pool, bot.ID, "51999444555", "Ana")
	if err != nil || contact == nil {
		t.Fatalf("EnsureInboundContact: %v", err)
	}

	base := time.Now().UTC().Truncate(time.Second)

	// Sin preferencia registrada no hay opt-out: no se bloquea a nadie por
	// defecto.
	if out, err := MarketingOptedOut(ctx, pool, contact.ID); err != nil || out {
		t.Fatalf("sin preferencia no debe haber opt-out: out=%v err=%v", out, err)
	}

	// Un stop bloquea.
	stop := prefEvento(contact.PhoneNormalized, whatsapp.PreferenceCategoryMarketing, whatsapp.PreferenceStop, base)
	if applied, err := StoreAndApplyUserPreference(ctx, pool, contact.ID, stop); err != nil || !applied {
		t.Fatalf("stop: applied=%v err=%v", applied, err)
	}
	if out, err := MarketingOptedOut(ctx, pool, contact.ID); err != nil || !out {
		t.Fatalf("el stop no bloqueó: out=%v err=%v", out, err)
	}

	// El reintento del mismo evento es idempotente.
	if applied, err := StoreAndApplyUserPreference(ctx, pool, contact.ID, stop); err != nil || applied {
		t.Fatalf("el reintento debía descartarse: applied=%v err=%v", applied, err)
	}

	// **La guarda que de verdad importa**: un `resume` atrasado no puede
	// resucitar los envíos de quien ya dijo que no.
	atrasado := prefEvento(contact.PhoneNormalized, whatsapp.PreferenceCategoryMarketing,
		whatsapp.PreferenceResume, base.Add(-time.Hour))
	if _, err := StoreAndApplyUserPreference(ctx, pool, contact.ID, atrasado); err != nil {
		t.Fatalf("resume atrasado: %v", err)
	}
	if out, err := MarketingOptedOut(ctx, pool, contact.ID); err != nil || !out {
		t.Fatalf("un resume atrasado revirtió el opt-out: out=%v err=%v", out, err)
	}

	// Un resume posterior sí lo levanta.
	nuevo := prefEvento(contact.PhoneNormalized, whatsapp.PreferenceCategoryMarketing,
		whatsapp.PreferenceResume, base.Add(time.Hour))
	if applied, err := StoreAndApplyUserPreference(ctx, pool, contact.ID, nuevo); err != nil || !applied {
		t.Fatalf("resume nuevo: applied=%v err=%v", applied, err)
	}
	if out, err := MarketingOptedOut(ctx, pool, contact.ID); err != nil || out {
		t.Fatalf("el resume posterior no levantó el bloqueo: out=%v err=%v", out, err)
	}

	// La bitácora conserva los tres eventos aplicables: es un dato de
	// cumplimiento, y la proyección sola habría perdido el historial del stop.
	var eventos int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM contact_preference_events WHERE contact_id=$1::uuid`,
		contact.ID).Scan(&eventos); err != nil {
		t.Fatalf("count: %v", err)
	}
	if eventos != 3 {
		t.Fatalf("esperaba 3 eventos en la bitácora, got %d", eventos)
	}

	prefs, err := ListContactPreferences(ctx, pool, contact.ID)
	if err != nil || len(prefs) != 1 {
		t.Fatalf("ListContactPreferences: n=%d err=%v", len(prefs), err)
	}
	if prefs[0].Value != whatsapp.PreferenceResume {
		t.Fatalf("preferencia vigente incorrecta: %+v", prefs[0])
	}
}

// Una categoría que Meta añada mañana debe guardarse igual, pero no puede
// bloquear el marketing por su cuenta.
func TestPreferenciaDeOtraCategoriaNoBloquea(t *testing.T) {
	pool := accountPool(t)
	defer pool.Close()
	ctx := context.Background()

	uid := randID("prefu2_")
	if _, err := pool.Exec(ctx, `INSERT INTO "user"(id,name,email,"emailVerified") VALUES ($1,$2,$3,false)`,
		uid, "Pref Owner", uid+"@test.local"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, uid)

	org, err := CreateOrganization(ctx, pool, uid, "Pref Org 2", nil, nil)
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	defer DeleteOrganization(ctx, pool, org.ID)

	bot, err := CreateBot(ctx, pool, org.ID, "Pref Bot 2", "wsp")
	if err != nil {
		t.Fatalf("CreateBot: %v", err)
	}
	contact, err := EnsureInboundContact(ctx, pool, bot.ID, "51999666777", "")
	if err != nil || contact == nil {
		t.Fatalf("EnsureInboundContact: %v", err)
	}

	otra := prefEvento(contact.PhoneNormalized, "categoria_futura", whatsapp.PreferenceStop, time.Now().UTC())
	if applied, err := StoreAndApplyUserPreference(ctx, pool, contact.ID, otra); err != nil || !applied {
		t.Fatalf("categoría desconocida: applied=%v err=%v", applied, err)
	}
	if out, err := MarketingOptedOut(ctx, pool, contact.ID); err != nil || out {
		t.Fatalf("una categoría ajena bloqueó el marketing: out=%v err=%v", out, err)
	}
	prefs, err := ListContactPreferences(ctx, pool, contact.ID)
	if err != nil || len(prefs) != 1 || prefs[0].Category != "categoria_futura" {
		t.Fatalf("la categoría desconocida no se guardó: %+v err=%v", prefs, err)
	}
}
