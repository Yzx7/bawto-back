package engine

import (
	"encoding/json"
	"os"
	"testing"
)

func loadWAACreditosFlow(t *testing.T) *Flow {
	t.Helper()
	raw, err := os.ReadFile("../db/flows/waa-creditos.json")
	if err != nil {
		t.Fatal(err)
	}
	var flow Flow
	if err := json.Unmarshal(raw, &flow); err != nil {
		t.Fatal(err)
	}
	return &flow
}

func TestWAACreditosFlowValidoYConToolDeRecarga(t *testing.T) {
	flow := loadWAACreditosFlow(t)
	if err := Validate(flow); err != nil {
		t.Fatalf("flow_waa_creditos inválido: %v", err)
	}

	nodes := map[string]*Node{}
	for i := range flow.Nodes {
		nodes[flow.Nodes[i].ID] = &flow.Nodes[i]
	}

	specialist := nodes["n_bawto_specialist"]
	activateTool := nodes["n_activate_subscription"]
	paymentNeedsReview := nodes["n_payment_needs_review"]
	paymentOK := nodes["n_payment_ok"]

	if specialist == nil || activateTool == nil || paymentNeedsReview == nil || paymentOK == nil {
		t.Fatalf("faltan nodos clave en flow_waa_creditos: specialist=%v activateTool=%v paymentNeedsReview=%v",
			specialist, activateTool, paymentNeedsReview)
	}

	// 1. Verificar especialista orientado a créditos
	if specialist.ToolRef != "" {
		t.Errorf("n_bawto_specialist no debe llamar herramientas directamente")
	}
	fields := map[string]string{}
	for _, f := range specialist.OutputFields {
		fields[f.Key] = f.Type
	}
	if fields["organizationCode"] != "string" {
		t.Errorf("n_bawto_specialist debe declarar organizationCode string: %+v", specialist.OutputFields)
	}
	if fields["creditsAmount"] != "number" {
		t.Errorf("n_bawto_specialist debe declarar creditsAmount number: %+v", specialist.OutputFields)
	}

	// 2. Verificar tool de recarga de créditos
	if activateTool.ToolRef != "credit_recharge_activate" {
		t.Errorf("n_activate_subscription debe usar toolRef credit_recharge_activate, tiene %q", activateTool.ToolRef)
	}
	if activateTool.Args["activationCode"] != "{sale.organizationCode}" {
		t.Errorf("n_activate_subscription.activationCode = %q, esperado {sale.organizationCode}", activateTool.Args["activationCode"])
	}
	if activateTool.Args["creditsAmount"] != "{sale.creditsAmount}" {
		t.Errorf("n_activate_subscription.creditsAmount = %q, esperado {sale.creditsAmount}", activateTool.Args["creditsAmount"])
	}

	// 3. Verificar condición de revisión de pago
	if paymentNeedsReview.Expression != "!empty(sale.organizationCode)" {
		t.Errorf("n_payment_needs_review.Expression = %q, esperado !empty(sale.organizationCode)", paymentNeedsReview.Expression)
	}
}
