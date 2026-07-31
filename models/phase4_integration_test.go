package models

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Yzx7/sacs-chatbots/channels/whatsapp"
)

func TestPhase4CatalogoYFechasSeguras(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "ph4_")
	wabaID := "waba-" + bot.ID
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM channel_template_events WHERE waba_id=$1`, wabaID)
		_, _ = pool.Exec(ctx, `DELETE FROM channel_templates WHERE waba_id=$1`, wabaID)
	})
	if _, err := pool.Exec(ctx, `UPDATE bots SET waba_id=$2 WHERE id=$1::uuid`, bot.ID, wabaID); err != nil {
		t.Fatalf("asociar WABA: %v", err)
	}

	object, err := CreateDataObjectByOrg(ctx, pool, bot.OrgID, "ordenes", "Orden", "Órdenes")
	if err != nil {
		t.Fatalf("CreateDataObject: %v", err)
	}
	if _, err = UpsertDataFieldByOrg(ctx, pool, bot.OrgID, object.ID, "vence", "Vence", "date", true); err != nil {
		t.Fatalf("UpsertDataField: %v", err)
	}
	valid, err := CreateDataRecordByOrg(ctx, pool, bot.OrgID, object.ID, json.RawMessage(`{"vence":"2026-07-31"}`))
	if err != nil {
		t.Fatalf("registro válido: %v", err)
	}
	for _, raw := range []string{
		`{"vence":"31/07/2026"}`, `{"vence":"2026-02-31"}`, `{"vence":""}`, `{"vence":null}`,
	} {
		if _, err = pool.Exec(ctx, `INSERT INTO data_records(object_id,data) VALUES($1::uuid,$2::jsonb)`, object.ID, raw); err != nil {
			t.Fatalf("insertar legado %s: %v", raw, err)
		}
	}
	days := 3
	filter, _ := json.Marshal(DataFilter{Where: []DataFilterRule{{
		Field: "vence", Op: "date_eq_relative", FromDays: &days,
	}}})
	view, err := CreateDataViewByOrg(ctx, pool, bot.OrgID, object.ID, "Vence D+3", filter)
	if err != nil {
		t.Fatalf("CreateDataView: %v", err)
	}
	lima, _ := time.LoadLocation("America/Lima")
	records, err := ResolveDataViewAt(ctx, pool, bot.ID, view.ID,
		time.Date(2026, 7, 28, 23, 59, 0, 0, lima))
	if err != nil {
		t.Fatalf("ResolveDataViewAt no debe abortar por fechas inválidas: %v", err)
	}
	if len(records) != 1 || records[0].ID != valid.ID {
		t.Fatalf("vista relativa inesperada: %+v", records)
	}

	components := json.RawMessage(`[{"type":"BODY","text":"Hola {{1}}, orden {{2}}"}]`)
	report, err := SyncChannelTemplates(ctx, pool, wabaID, []whatsapp.TemplateInfo{{
		ID: "meta-1", Name: "aviso_orden", Language: "es", Status: "APPROVED",
		Category: "UTILITY", ParameterFormat: "POSITIONAL", Components: components,
	}}, time.Now().UTC())
	if err != nil || report.Total != 1 {
		t.Fatalf("SyncChannelTemplates: report=%+v err=%v", report, err)
	}
	schedule := map[string]any{
		"id": "schedule_order", "name": "Aviso de orden",
		"trigger": map[string]any{
			"type": "schedule", "cron": "0 9 * * *", "timezone": "America/Lima", "viewId": view.ID,
		},
		"nodes": []any{
			map[string]any{"id": "n1", "kind": "send", "templateName": "aviso_orden",
				"templateLanguage": "es", "templateParams": []string{"{contact_name}", "{record_numero}"}},
			map[string]any{"id": "n2", "kind": "action", "action": "end"},
		},
		"edges": []any{
			map[string]any{"id": "e0", "source": "trigger", "target": "n1"},
			map[string]any{"id": "e1", "source": "n1", "target": "n2"},
		},
	}
	rawSchedule, _ := json.Marshal(schedule)
	validation, err := ValidateFlowTemplates(ctx, pool, bot.ID, rawSchedule)
	if err != nil || len(validation.Warnings) != 0 {
		t.Fatalf("validación UTILITY: result=%+v err=%v", validation, err)
	}

	categoryEvent := whatsapp.TemplateEvent{
		EventKey: "category-" + bot.ID, WabaID: wabaID, Field: "template_category_update",
		TemplateID: "meta-1", Name: "aviso_orden", Language: "es",
		OccurredAt: time.Now().Add(time.Minute).UTC(), Category: "MARKETING",
		Payload: json.RawMessage(`{"new_category":"MARKETING"}`),
	}
	applied, err := StoreAndApplyTemplateEvent(ctx, pool, categoryEvent)
	if err != nil || !applied {
		t.Fatalf("StoreAndApplyTemplateEvent: applied=%v err=%v", applied, err)
	}
	applied, err = StoreAndApplyTemplateEvent(ctx, pool, categoryEvent)
	if err != nil || applied {
		t.Fatalf("el evento duplicado debe ser no-op: applied=%v err=%v", applied, err)
	}
	validation, err = ValidateFlowTemplates(ctx, pool, bot.ID, rawSchedule)
	if err != nil || len(validation.Warnings) != 1 {
		t.Fatalf("MARKETING debe advertir sin bloquear: result=%+v err=%v", validation, err)
	}
}
