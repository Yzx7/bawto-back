package models

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRenderPaymentInstructionsIncludesEveryMethodOnce(t *testing.T) {
	message := renderPaymentInstructions([]paymentMethod{
		{Medium: "Yape", Destination: "999 111 222", Holder: "Sistemuino SAC", Currency: "PEN"},
		{Medium: "Cuenta BCP", Destination: "191-0000000-0-00", Holder: "Sistemuino SAC", Currency: "PEN", Note: "Cuenta corriente."},
	})
	for _, expected := range []string{
		"1. Yape", "Destino: 999 111 222", "2. Cuenta BCP",
		"Destino: 191-0000000-0-00", "Cuenta corriente.", "captura completa y nítida",
	} {
		if strings.Count(message, expected) != 1 {
			t.Fatalf("%q debe aparecer una vez en %q", expected, message)
		}
	}
}

func TestRenderPaymentInstructionsUsesSingularForOneMethod(t *testing.T) {
	message := renderPaymentInstructions([]paymentMethod{{
		Medium: "Plin", Destination: "999", Holder: "Sistemuino", Currency: "PEN",
	}})
	if !strings.Contains(message, "con este medio") || strings.Contains(message, "uno de estos medios") {
		t.Fatalf("encabezado singular inesperado: %q", message)
	}
}

func TestPaymentInstructionsForOrgReadsAllActiveMethodsInPriorityOrder(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "payments_")
	seedPaymentMethods(t, ctx, pool, bot.OrgID)

	result, err := PaymentInstructionsForOrg(ctx, pool, bot.OrgID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Found || result.Count != 2 {
		t.Fatalf("found=%v count=%d", result.Found, result.Count)
	}
	yape := strings.Index(result.Message, "1. Yape")
	bcp := strings.Index(result.Message, "2. Cuenta BCP")
	if yape < 0 || bcp < 0 || yape >= bcp {
		t.Fatalf("orden inesperado: %q", result.Message)
	}
	if strings.Contains(result.Message, "Plin pausado") {
		t.Fatal("un método inactivo no puede llegar al cliente")
	}
	if _, err = MutateDataRecord(ctx, pool, DataMutationInput{
		OrgID: bot.OrgID, ObjectKey: PlatformPaymentMethodsObject, Operation: "create",
		Values: map[string]any{
			"clave": "yape", "medio": "Yape duplicado", "destino": "777",
			"titular": "Sistemuino", "moneda": "PEN", "prioridad": 30, "activo": true,
		},
		IdempotencyKey: randID("duplicate_payment_method_"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = PaymentInstructionsForOrg(ctx, pool, bot.OrgID); err == nil || !strings.Contains(err.Error(), "más de un método") {
		t.Fatalf("un duplicado activo debe fallar de forma segura: %v", err)
	}
}

func seedPaymentMethods(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID string) {
	t.Helper()
	object, err := CreateDataObjectByOrg(ctx, pool, orgID, PlatformPaymentMethodsObject, "Método", "Métodos")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ key, typ string }{
		{"clave", "text"}, {"medio", "text"}, {"destino", "text"}, {"titular", "text"},
		{"moneda", "text"}, {"nota", "text"}, {"prioridad", "number"}, {"activo", "boolean"},
	} {
		if _, err = UpsertDataFieldByOrg(ctx, pool, orgID, object.ID, item.key, item.key, item.typ, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, values := range []map[string]any{
		{"clave": "bcp", "medio": "Cuenta BCP", "destino": "191", "titular": "Sistemuino", "moneda": "PEN", "prioridad": 20, "activo": true},
		{"clave": "yape", "medio": "Yape", "destino": "999", "titular": "Sistemuino", "moneda": "PEN", "prioridad": 10, "activo": true},
		{"clave": "plin", "medio": "Plin pausado", "destino": "888", "titular": "Sistemuino", "moneda": "PEN", "prioridad": 1, "activo": false},
	} {
		if _, err = MutateDataRecord(ctx, pool, DataMutationInput{
			OrgID: orgID, ObjectKey: PlatformPaymentMethodsObject, Operation: "create",
			Values: values, IdempotencyKey: randID("payment_method_"),
		}); err != nil {
			t.Fatal(err)
		}
	}
}
