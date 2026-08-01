package models

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPhase3StatusDurableYCorrelacionSegura(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "p3_")

	contact, err := SaveContactByOrg(ctx, pool, bot.OrgID, "", "51999111222", "Ana", "active", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	object, err := CreateDataObjectByOrg(ctx, pool, bot.OrgID, "ordenes", "Orden", "Órdenes")
	if err != nil {
		t.Fatal(err)
	}
	record, err := CreateDataRecordByOrg(ctx, pool, bot.OrgID, object.ID, json.RawMessage(`{"numero":"O-1","estado":"pendiente"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := LinkRecordContactByOrg(ctx, pool, bot.OrgID, record.ID, contact.ID, "primary"); err != nil {
		t.Fatal(err)
	}

	raw := grafoValido("flow-p3", "P3", "hola")
	var definition map[string]any
	if err := json.Unmarshal(raw, &definition); err != nil {
		t.Fatal(err)
	}
	definition["trigger"].(map[string]any)["replyIntent"] = "payment_reminder_reply"
	raw, _ = json.Marshal(definition)
	flow, err := CreateFlow(ctx, pool, bot.ID, NewFlow{
		Key: "phase3", Name: "Phase 3", TriggerType: "message", Draft: raw, UserID: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	published := publica(t, ctx, pool, bot.ID, flow.ID, raw)

	run, created, err := CreateFlowRun(ctx, pool, bot.ID, flow.ID, published.Version.ID,
		record.ID, contact.ID, "p3:"+randID(""), "schedule", time.Now().UTC(), json.RawMessage(`{}`))
	if err != nil || !created {
		t.Fatalf("CreateFlowRun: created=%v err=%v", created, err)
	}
	providerID := "wamid." + randID("p3")
	if _, err := pool.Exec(ctx, `UPDATE flow_runs SET status='sent',provider_message_id=$2 WHERE id=$1::uuid`, run.ID, providerID); err != nil {
		t.Fatal(err)
	}

	base := time.Now().UTC().Add(-time.Minute)
	// Llegan antes del mensaje y en orden de recepción inverso.
	for _, event := range []ProviderStatusEventInput{
		{Channel: "wsp", ProviderMessageID: providerID, Status: "read", OccurredAt: base.Add(2 * time.Second), PricingModel: "PMP", PricingType: "regular", PricingCategory: "utility"},
		{Channel: "wsp", ProviderMessageID: providerID, Status: "delivered", OccurredAt: base.Add(time.Second), ConversationID: "conv-p3"},
	} {
		if _, err := StoreProviderStatusEvent(ctx, pool, event); err != nil {
			t.Fatal(err)
		}
	}
	if updates, err := ReconcileProviderStatusEvents(ctx, pool, providerID); err != nil || len(updates) != 0 {
		t.Fatalf("sin message debe quedar pendiente: updates=%v err=%v", updates, err)
	}

	chatID, err := UpsertChat(ctx, pool, bot.ID, contact.PhoneNormalized, "Ana")
	if err != nil {
		t.Fatal(err)
	}
	outbound, err := InsertOutboundMessageWithMetadata(ctx, pool, chatID, providerID, "template", "aviso", json.RawMessage(`{}`))
	if err != nil || outbound == nil {
		t.Fatalf("insert outbound: message=%v err=%v", outbound, err)
	}
	updates, err := ReconcileProviderStatusEvents(ctx, pool, providerID)
	if err != nil || len(updates) != 2 || updates[len(updates)-1].Status != "read" {
		t.Fatalf("reconcile: updates=%+v err=%v", updates, err)
	}
	var runStatus, conversationID, pricingModel, pricingType, pricingCategory string
	if err := pool.QueryRow(ctx, `SELECT status,conversation_id,pricing_model,pricing_type,pricing_category
		FROM flow_runs WHERE id=$1::uuid`, run.ID).
		Scan(&runStatus, &conversationID, &pricingModel, &pricingType, &pricingCategory); err != nil {
		t.Fatal(err)
	}
	if runStatus != "read" || conversationID != "conv-p3" || pricingModel != "PMP" ||
		pricingType != "regular" || pricingCategory != "utility" {
		t.Fatalf("run no reconciliado: %s %s %s %s %s",
			runStatus, conversationID, pricingModel, pricingType, pricingCategory)
	}

	inboundID, _, err := InsertInboundMessage(ctx, pool, chatID, "wamid.in."+randID(""),
		"text", "ya pagué", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	correlation, err := CorrelateInboundReminder(ctx, pool, inboundID, chatID, providerID, 72*time.Hour)
	if err != nil || correlation.Method != "exact" || correlation.DataRecordID == nil || *correlation.DataRecordID != record.ID {
		t.Fatalf("correlación exacta incorrecta: %+v err=%v", correlation, err)
	}
	contextVars, err := CorrelationContext(ctx, pool, bot.ID, correlation)
	if err != nil || contextVars["record_numero"] != "O-1" ||
		contextVars["data_ordenes_numero"] != "O-1" ||
		contextVars["source_intent"] != "payment_reminder_reply" {
		t.Fatalf("contexto correlacionado incorrecto: %+v err=%v", contextVars, err)
	}

	inferredInboundID, _, err := InsertInboundMessage(ctx, pool, chatID, "wamid.in."+randID(""),
		"text", "te respondo sin citar", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	inferred, err := CorrelateInboundReminder(ctx, pool, inferredInboundID, chatID, "", 72*time.Hour)
	if err != nil || inferred.Method != "inferred" || inferred.CandidateCount != 1 ||
		inferred.DataRecordID == nil || *inferred.DataRecordID != record.ID {
		t.Fatalf("fallback único incorrecto: %+v err=%v", inferred, err)
	}

	secondRecord, err := CreateDataRecordByOrg(ctx, pool, bot.OrgID, object.ID,
		json.RawMessage(`{"numero":"O-2","estado":"pendiente"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := LinkRecordContactByOrg(ctx, pool, bot.OrgID, secondRecord.ID, contact.ID, "primary"); err != nil {
		t.Fatal(err)
	}
	secondRun, created, err := CreateFlowRun(ctx, pool, bot.ID, flow.ID, published.Version.ID,
		secondRecord.ID, contact.ID, "p3:"+randID(""), "schedule", time.Now().UTC(), json.RawMessage(`{}`))
	if err != nil || !created {
		t.Fatalf("segundo CreateFlowRun: created=%v err=%v", created, err)
	}
	secondProviderID := "wamid." + randID("p3")
	if _, err := pool.Exec(ctx, `UPDATE flow_runs SET status='sent',provider_message_id=$2
		WHERE id=$1::uuid`, secondRun.ID, secondProviderID); err != nil {
		t.Fatal(err)
	}
	if message, err := InsertOutboundMessageWithMetadata(ctx, pool, chatID, secondProviderID,
		"template", "segundo aviso", json.RawMessage(`{}`)); err != nil || message == nil {
		t.Fatalf("segundo outbound: message=%v err=%v", message, err)
	}
	ambiguousInboundID, _, err := InsertInboundMessage(ctx, pool, chatID, "wamid.in."+randID(""),
		"image", "comprobante", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	ambiguous, err := CorrelateInboundReminder(ctx, pool, ambiguousInboundID, chatID, "", 72*time.Hour)
	if err != nil || ambiguous.Method != "ambiguous" || ambiguous.CandidateCount != 2 ||
		ambiguous.DataRecordID != nil {
		t.Fatalf("atribución ambigua incorrecta: %+v err=%v", ambiguous, err)
	}
}
