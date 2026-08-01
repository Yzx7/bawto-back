package engine

import (
	"encoding/json"
	"os"
	"testing"
)

func loadWAAFlow(t *testing.T) *Flow {
	t.Helper()

	raw, err := os.ReadFile("../db/flows/waa.json")
	if err != nil {
		t.Fatal(err)
	}

	var flow Flow
	if err = json.Unmarshal(raw, &flow); err != nil {
		t.Fatal(err)
	}
	return &flow
}

func waaTestAgent(request AgentRequest) (string, string, error) {
	vars, outputs := request.Vars, request.Outputs
	if contains(outputs, "billing") {
		switch vars["input"] {
		case "pago":
			return "", "payment", nil
		case "sin internet":
			return "", "no_internet", nil
		default:
			return "", "menu", nil
		}
	}
	if contains(outputs, "los") {
		if vars["input"] == "1" {
			return "", "resolved", nil
		}
		return "", "los", nil
	}
	if contains(outputs, "escalate") {
		if vars["input"] == "si" {
			return "", "resolved", nil
		}
		return "", "escalate", nil
	}
	return "", outputs[0], nil
}

func TestWAAFlowIsValid(t *testing.T) {
	flow := loadWAAFlow(t)
	if err := Validate(flow); err != nil {
		t.Fatalf("WAA inválido: %v", err)
	}
}

func TestWAAMenuAndTechnicalJourneys(t *testing.T) {
	flow := loadWAAFlow(t)
	deps := Deps{InputType: "text", Agent: waaTestAgent}

	menu, err := Advance(flow, nil, "hola", deps)
	if err != nil || menu.State == nil || menu.State.NodeID != "n_wait_menu" || len(menu.Sends) != 1 {
		t.Fatalf("menú: %+v err=%v", menu, err)
	}

	diagnosis, err := Advance(flow, nil, "sin internet", deps)
	if err != nil || diagnosis.State == nil || diagnosis.State.NodeID != "n_wait_nointernet" {
		t.Fatalf("inicio diagnóstico: %+v err=%v", diagnosis, err)
	}

	resolved, err := Advance(flow, diagnosis.State, "1", deps)
	if err != nil || !resolved.Done || resolved.Handoff || len(resolved.Sends) == 0 {
		t.Fatalf("diagnóstico resuelto: %+v err=%v", resolved, err)
	}

	escalated, err := Advance(flow, diagnosis.State, "2", deps)
	if err != nil || !escalated.Done || !escalated.Handoff || len(escalated.Sends) < 2 {
		t.Fatalf("diagnóstico LOS: %+v err=%v", escalated, err)
	}
}

func TestWAAPaymentJourneys(t *testing.T) {
	flow := loadWAAFlow(t)
	deps := Deps{InputType: "text", Agent: waaTestAgent}

	withoutInvoice, err := Advance(flow, nil, "pago", deps)
	if err != nil || withoutInvoice.State == nil || withoutInvoice.State.NodeID != "n_wait_payment_identifier" {
		t.Fatalf("pago sin factura vinculada: %+v err=%v", withoutInvoice, err)
	}

	invoiceContext := map[string]string{
		"data_facturas_numero":      "FAC-2026-002",
		"data_facturas_periodo":     "Julio 2026",
		"data_facturas_vencimiento": "2026-07-31",
		"data_facturas_importe":     "89.90",
		"data_facturas_moneda":      "PEN",
		"data_facturas_estado":      "pendiente",
	}
	withInvoice, err := Advance(flow, nil, "pago", Deps{
		InputType: "text",
		Agent:     waaTestAgent,
		Context:   invoiceContext,
	})
	if err != nil || withInvoice.State == nil || withInvoice.State.NodeID != "n_wait_receipt" {
		t.Fatalf("pago con factura: %+v err=%v", withInvoice, err)
	}

	toolCalled := false
	recorded, err := Advance(flow, withInvoice.State, "comprobante", Deps{
		InputType: "image",
		MediaID:   "media-waa",
		WaID:      "wamid-waa",
		Agent:     waaTestAgent,
		Context:   invoiceContext,
		Tool: func(ref string, _ map[string]string, vars map[string]string) (string, error) {
			toolCalled = ref == "record_payment_receipt" &&
				vars["payment_receipt"] == "media-waa" &&
				vars["input_wa_id"] == "wamid-waa"
			return "invoice-id", nil
		},
	})
	if err != nil || !toolCalled || !recorded.Done || recorded.Handoff || len(recorded.Sends) == 0 {
		t.Fatalf("comprobante: called=%v result=%+v err=%v", toolCalled, recorded, err)
	}
}
