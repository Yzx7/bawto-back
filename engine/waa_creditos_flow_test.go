package engine

import (
	"encoding/json"
	"os"
	"strings"
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
	classifyPayment := nodes["n_classify_payment"]
	savePaymentReview := nodes["n_save_payment_review"]
	activateTool := nodes["n_activate_subscription"]
	paymentNeedsReview := nodes["n_payment_needs_review"]
	paymentOK := nodes["n_payment_ok"]
	paymentReview := nodes["n_payment_review"]

	if specialist == nil || classifyPayment == nil || savePaymentReview == nil || activateTool == nil || paymentNeedsReview == nil || paymentOK == nil || paymentReview == nil {
		t.Fatalf("faltan nodos clave en flow_waa_creditos")
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

	// 2. Verificar instrucción de n_classify_payment con validación de destinatario
	if !strings.Contains(classifyPayment.Instruction, "Gerson Rodriguez") || !strings.Contains(classifyPayment.Instruction, "973021342") {
		t.Errorf("n_classify_payment debe incluir cuentas autorizadas de Sistemuino / Bawto en su prompt")
	}
	if !strings.Contains(classifyPayment.Instruction, "needs_review") {
		t.Errorf("n_classify_payment debe derivar a needs_review si el destinatario no coincide")
	}

	// 3. Verificar n_save_payment_review
	if savePaymentReview.Args["field.estado"] != "pendiente" {
		t.Errorf("n_save_payment_review debe guardar estado: pendiente, tiene %q", savePaymentReview.Args["field.estado"])
	}

	// 4. Verificar tool de recarga de créditos
	if activateTool.ToolRef != "credit_recharge_activate" {
		t.Errorf("n_activate_subscription debe usar toolRef credit_recharge_activate, tiene %q", activateTool.ToolRef)
	}
	if activateTool.Args["activationCode"] != "{sale.organizationCode}" {
		t.Errorf("n_activate_subscription.activationCode = %q, esperado {sale.organizationCode}", activateTool.Args["activationCode"])
	}
	if activateTool.Args["creditsAmount"] != "{sale.creditsAmount}" {
		t.Errorf("n_activate_subscription.creditsAmount = %q, esperado {sale.creditsAmount}", activateTool.Args["creditsAmount"])
	}

	// 5. Verificar condición de revisión de pago
	if paymentNeedsReview.Expression != "!empty(sale.organizationCode)" {
		t.Errorf("n_payment_needs_review.Expression = %q, esperado !empty(sale.organizationCode)", paymentNeedsReview.Expression)
	}

	// 6. Verificar edges
	edges := map[string][]string{}
	for _, e := range flow.Edges {
		edges[e.Source+"."+e.SourceHandle] = append(edges[e.Source+"."+e.SourceHandle], e.Target)
	}
	if targets := edges["n_classify_payment.needs_review"]; len(targets) == 0 || targets[0] != "n_save_payment_review" {
		t.Errorf("n_classify_payment.needs_review debe conectar a n_save_payment_review, tiene %v", targets)
	}
	if targets := edges["n_save_payment_review.ok"]; len(targets) == 0 || targets[0] != "n_payment_review" {
		t.Errorf("n_save_payment_review.ok debe conectar a n_payment_review, tiene %v", targets)
	}
}
