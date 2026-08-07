package models

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yzx7/sacs-chatbots/channels/whatsapp"
)

func accountPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL no seteada; se omite el test de integración")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	return pool
}

func evento(wabaID, field, ev, limit, decision, severity string, at time.Time) whatsapp.AccountEvent {
	return whatsapp.AccountEvent{
		WabaID: wabaID, Field: field, Event: ev, MessagingLimit: limit,
		Decision: decision, Severity: severity, OccurredAt: at,
		EventKey: randID("evk_"), Payload: json.RawMessage(`{"x":1}`),
	}
}

func TestStoreAndApplyAccountEvent(t *testing.T) {
	pool := accountPool(t)
	defer pool.Close()
	ctx := context.Background()

	waba := randID("waba_")
	defer pool.Exec(ctx, `DELETE FROM channel_account_events WHERE waba_id=$1`, waba)
	defer pool.Exec(ctx, `DELETE FROM channel_health WHERE waba_id=$1`, waba)

	base := time.Now().UTC().Truncate(time.Second)

	// 1) Un evento de calidad escribe sus columnas.
	e1 := evento(waba, "phone_number_quality_update", "FLAGGED", "TIER_1K", "", whatsapp.SeverityWarning, base)
	e1.PhoneNumberID = "PN1"
	if applied, err := StoreAndApplyAccountEvent(ctx, pool, e1); err != nil || !applied {
		t.Fatalf("primer evento: applied=%v err=%v", applied, err)
	}

	// 2) El reintento del mismo evento es idempotente.
	repetido := e1
	if applied, err := StoreAndApplyAccountEvent(ctx, pool, repetido); err != nil || applied {
		t.Fatalf("el reintento debía descartarse: applied=%v err=%v", applied, err)
	}

	// 3) Otro campo escribe **solo sus columnas** y no borra las anteriores.
	e2 := evento(waba, "phone_number_name_update", "", "", "REJECTED", whatsapp.SeverityWarning, base.Add(time.Second))
	e2.PhoneNumberID = "PN1"
	if applied, err := StoreAndApplyAccountEvent(ctx, pool, e2); err != nil || !applied {
		t.Fatalf("segundo evento: applied=%v err=%v", applied, err)
	}

	health := healthDe(t, ctx, pool, waba, "PN1")
	if health.QualityEvent == nil || *health.QualityEvent != "FLAGGED" {
		t.Fatalf("un evento de nombre borró la calidad: %+v", health)
	}
	if health.MessagingLimit == nil || *health.MessagingLimit != "TIER_1K" {
		t.Fatalf("se perdió el límite: %+v", health)
	}
	if health.NameDecision == nil || *health.NameDecision != "REJECTED" {
		t.Fatalf("no se aplicó la decisión de nombre: %+v", health)
	}

	// 4) La guarda de orden: un webhook atrasado no revierte un estado más nuevo.
	// Los eventos de Meta no llegan ordenados; sin esto un FLAGGED viejo pisaría
	// un UNFLAGGED reciente y la alarma quedaría encendida para siempre.
	nuevo := evento(waba, "phone_number_quality_update", "UNFLAGGED", "TIER_10K", "", whatsapp.SeverityInfo, base.Add(10*time.Second))
	nuevo.PhoneNumberID = "PN1"
	if applied, err := StoreAndApplyAccountEvent(ctx, pool, nuevo); err != nil || !applied {
		t.Fatalf("evento nuevo: applied=%v err=%v", applied, err)
	}
	atrasado := evento(waba, "phone_number_quality_update", "FLAGGED", "TIER_1K", "", whatsapp.SeverityWarning, base.Add(2*time.Second))
	atrasado.PhoneNumberID = "PN1"
	if _, err := StoreAndApplyAccountEvent(ctx, pool, atrasado); err != nil {
		t.Fatalf("evento atrasado: %v", err)
	}

	health = healthDe(t, ctx, pool, waba, "PN1")
	if health.QualityEvent == nil || *health.QualityEvent != "UNFLAGGED" {
		t.Fatalf("un webhook atrasado revirtió el estado: %+v", health)
	}
	if health.Severity != whatsapp.SeverityInfo {
		t.Fatalf("la severidad la fijó el evento atrasado: %q", health.Severity)
	}

	// El atrasado sí queda en la bitácora: se descarta para el estado, no para
	// el historial.
	eventos, err := ListChannelAccountEvents(ctx, pool, waba, false, 50)
	if err != nil {
		t.Fatalf("ListChannelAccountEvents: %v", err)
	}
	if len(eventos) != 4 {
		t.Fatalf("esperaba 4 eventos en la bitácora, got %d", len(eventos))
	}

	// El filtro de problemas omite los informativos.
	problemas, err := ListChannelAccountEvents(ctx, pool, waba, true, 50)
	if err != nil {
		t.Fatalf("ListChannelAccountEvents(problemas): %v", err)
	}
	if len(problemas) != 3 {
		t.Fatalf("esperaba 3 problemas (se excluye el info), got %d", len(problemas))
	}
}

// Un evento de cuenta no trae número. Debe ocupar su propia fila y no duplicarse
// en cada llegada: es para lo que está NULLS NOT DISTINCT.
func TestStoreAndApplyAccountEventSinNumero(t *testing.T) {
	pool := accountPool(t)
	defer pool.Close()
	ctx := context.Background()

	waba := randID("waba_")
	defer pool.Exec(ctx, `DELETE FROM channel_account_events WHERE waba_id=$1`, waba)
	defer pool.Exec(ctx, `DELETE FROM channel_health WHERE waba_id=$1`, waba)

	base := time.Now().UTC().Truncate(time.Second)
	for i, ev := range []string{"PRIMERO", "SEGUNDO"} {
		e := evento(waba, "account_update", ev, "", "", whatsapp.SeverityCritical, base.Add(time.Duration(i)*time.Second))
		if applied, err := StoreAndApplyAccountEvent(ctx, pool, e); err != nil || !applied {
			t.Fatalf("%s: applied=%v err=%v", ev, applied, err)
		}
	}

	var filas int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM channel_health WHERE waba_id=$1 AND phone_number_id IS NULL`,
		waba).Scan(&filas); err != nil {
		t.Fatalf("count: %v", err)
	}
	if filas != 1 {
		t.Fatalf("el evento sin número duplicó filas: %d", filas)
	}

	rows, err := GetChannelHealth(ctx, pool, waba)
	if err != nil {
		t.Fatalf("GetChannelHealth: %v", err)
	}
	if len(rows) != 1 || rows[0].AccountEvent == nil || *rows[0].AccountEvent != "SEGUNDO" {
		t.Fatalf("estado inesperado: %+v", rows)
	}
	if rows[0].Severity != whatsapp.SeverityCritical {
		t.Fatalf("severidad perdida: %q", rows[0].Severity)
	}
}

func healthDe(t *testing.T, ctx context.Context, pool *pgxpool.Pool, waba, phone string) ChannelHealth {
	t.Helper()
	rows, err := GetChannelHealth(ctx, pool, waba)
	if err != nil {
		t.Fatalf("GetChannelHealth: %v", err)
	}
	for _, r := range rows {
		if r.PhoneNumberID != nil && *r.PhoneNumberID == phone {
			return r
		}
	}
	t.Fatalf("no se encontró salud para %s: %+v", phone, rows)
	return ChannelHealth{}
}
