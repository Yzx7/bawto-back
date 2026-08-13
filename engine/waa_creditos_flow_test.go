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
	savePayment := nodes["n_save_payment"]
	activateTool := nodes["n_activate_subscription"]
	paymentNeedsReview := nodes["n_payment_needs_review"]
	askOrgCode := nodes["n_ask_org_code"]
	existingReceiptRouter := nodes["n_existing_receipt_router"]
	receiptCompleteRouter := nodes["n_receipt_complete_router"]
	receiptReviewableRouter := nodes["n_receipt_reviewable_router"]
	paymentSpecialist := nodes["n_payment_specialist"]
	paymentOK := nodes["n_payment_ok"]
	paymentReview := nodes["n_payment_review"]

	if specialist == nil || classifyPayment == nil || savePaymentReview == nil || savePayment == nil ||
		activateTool == nil || paymentNeedsReview == nil || askOrgCode == nil || existingReceiptRouter == nil ||
		receiptCompleteRouter == nil || receiptReviewableRouter == nil || paymentSpecialist == nil || paymentOK == nil || paymentReview == nil {
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
	if _, exists := fields["creditsAmount"]; exists {
		t.Errorf("la IA no debe decidir cuántos créditos acreditar: %+v", specialist.OutputFields)
	}

	// 2. Verificar instrucción de n_classify_payment con validación de destinatario
	if !strings.Contains(classifyPayment.Instruction, "{authorized_payment_methods}") {
		t.Errorf("n_classify_payment debe leer las cuentas autorizadas de la fuente de verdad")
	}
	if strings.Contains(classifyPayment.Instruction, "973021342") || strings.Contains(classifyPayment.Instruction, "Gerson Rodriguez") {
		t.Errorf("n_classify_payment no debe congelar cuentas de pago en el prompt")
	}
	if !strings.Contains(classifyPayment.Instruction, "needs_review") {
		t.Errorf("n_classify_payment debe derivar a needs_review si el destinatario no coincide")
	}
	for _, field := range []string{"provider", "amount", "currency", "occurredAt", "operationCode", "recipient"} {
		if !strings.Contains(classifyPayment.Instruction, field) {
			t.Errorf("n_classify_payment no declara %s como obligatorio para valid", field)
		}
	}
	if strings.Contains(askOrgCode.Body, "recibido exitosamente") {
		t.Errorf("n_ask_org_code no debe afirmar éxito antes de acreditar")
	}
	if strings.Contains(strings.Join(specialist.ReplyOn, ","), "cobrar") || strings.Contains(strings.Join(paymentSpecialist.ReplyOn, ","), "cobrar") {
		t.Errorf("los especialistas no deben responder antes de ejecutar la rama cobrar")
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
	if activateTool.Args["creditsAmount"] != "" || activateTool.Args["amount"] != "" || activateTool.Args["idempotencyKey"] != "" {
		t.Errorf("el flujo no debe decidir monto ni idempotencia de la recarga: %+v", activateTool.Args)
	}

	// 5. Verificar condición de revisión / petición de código
	if paymentNeedsReview.Expression != "!empty(sale.organizationCode)" {
		t.Errorf("n_payment_needs_review.Expression = %q, esperado !empty(sale.organizationCode)", paymentNeedsReview.Expression)
	}

	// 6. Verificar aristas del flujo de código
	edges := map[string][]string{}
	for _, e := range flow.Edges {
		edges[e.Source+"."+e.SourceHandle] = append(edges[e.Source+"."+e.SourceHandle], e.Target)
	}
	if targets := edges["n_classify_payment.valid"]; len(targets) == 0 || targets[0] != "n_receipt_complete_router" {
		t.Errorf("n_classify_payment.valid debe validar campos obligatorios, tiene %v", targets)
	}
	if targets := edges["n_receipt_complete_router.complete"]; len(targets) == 0 || targets[0] != "n_payment_needs_review" {
		t.Errorf("solo un comprobante completo debe continuar hacia la recarga, tiene %v", targets)
	}
	if targets := edges["n_receipt_complete_router.default"]; len(targets) == 0 || targets[0] != "n_payment_retry" {
		t.Errorf("un valid incompleto debe pedir otra captura, tiene %v", targets)
	}
	if targets := edges["n_classify_payment.needs_review"]; len(targets) == 0 || targets[0] != "n_receipt_reviewable_router" {
		t.Errorf("needs_review debe comprobar si hay datos suficientes para registrar, tiene %v", targets)
	}
	if targets := edges["n_receipt_reviewable_router.reviewable"]; len(targets) == 0 || targets[0] != "n_save_payment_review" {
		t.Errorf("un comprobante revisable debe guardarse pendiente, tiene %v", targets)
	}
	if targets := edges["n_receipt_reviewable_router.default"]; len(targets) == 0 || targets[0] != "n_payment_retry" {
		t.Errorf("un comprobante insuficiente no debe crear un cobro, tiene %v", targets)
	}
	if targets := edges["n_payment_needs_review.true"]; len(targets) == 0 || targets[0] != "n_save_payment" {
		t.Errorf("n_payment_needs_review.true debe conectar a n_save_payment, tiene %v", targets)
	}
	if targets := edges["n_payment_needs_review.false"]; len(targets) == 0 || targets[0] != "n_ask_org_code" {
		t.Errorf("n_payment_needs_review.false debe conectar a n_ask_org_code, tiene %v", targets)
	}
	if targets := edges["n_ask_org_code.out"]; len(targets) == 0 || targets[0] != "n_espera" {
		t.Errorf("n_ask_org_code.out debe conectar a n_espera, tiene %v", targets)
	}
	if targets := edges["n_bawto_specialist.cobrar"]; len(targets) == 0 || targets[0] != "n_existing_receipt_router" {
		t.Errorf("la rama cobrar debe reutilizar un comprobante previo, tiene %v", targets)
	}
	if targets := edges["n_existing_receipt_router.ready"]; len(targets) == 0 || targets[0] != "n_save_payment" {
		t.Errorf("comprobante previo + código deben guardarse sin pedir otra imagen, tiene %v", targets)
	}
	if targets := edges["n_existing_receipt_router.incomplete"]; len(targets) == 0 || targets[0] != "n_payment_retry" {
		t.Errorf("un comprobante previo incompleto no debe volver a mostrar medios de pago, tiene %v", targets)
	}
}

func TestWAACreditosValidSinMontoNoGuardaNiPrometeRecarga(t *testing.T) {
	flow := loadWAACreditosFlow(t)
	toolCalls := []string{}
	result, err := Advance(flow, nil, "", Deps{
		InputType: "image",
		WaID:      "wamid-valid-sin-monto",
		AgentStructured: func(request AgentRequest) (AgentResult, error) {
			if request.NodeID != "n_classify_payment" {
				t.Fatalf("agente inesperado: %s", request.NodeID)
			}
			return AgentResult{Branch: "valid", Data: map[string]any{
				"provider": "bcp", "currency": "PEN",
				"occurredAt":    "2026-08-13T02:23:00-05:00",
				"operationCode": "01391991", "recipient": "Gerson Rodriguez",
			}}, nil
		},
		Tool: func(ref string, _ map[string]string, _ map[string]string) (string, error) {
			toolCalls = append(toolCalls, ref)
			if ref != "data_query" {
				t.Fatalf("un valid incompleto no debe ejecutar %s", ref)
			}
			return `{"found":true,"count":1,"first":{"recordId":"method","data":{"activo":true,"destino":"973021342","titular":"Gerson Rodriguez"}}}`, nil
		},
	})
	if err != nil {
		t.Fatalf("avance con extracción incompleta: %v", err)
	}
	if len(toolCalls) != 1 || toolCalls[0] != "data_query" {
		t.Fatalf("herramientas inesperadas: %v", toolCalls)
	}
	if result.State == nil || result.State.NodeID != "n_espera" || len(result.Sends) != 1 {
		t.Fatalf("resultado inesperado: %+v", result)
	}
	if strings.Contains(result.Sends[0], "{receipt.amount}") || strings.Contains(result.Sends[0], "créditos acreditados") {
		t.Fatalf("respuesta insegura: %q", result.Sends[0])
	}
}
