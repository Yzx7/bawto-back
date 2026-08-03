package engine

import (
	"encoding/json"
	"os"
	"testing"
)

func loadSistemuinoAgentFlow(t *testing.T) *Flow {
	t.Helper()
	raw, err := os.ReadFile("../db/flows/sistemuino-agente.json")
	if err != nil {
		t.Fatal(err)
	}
	var flow Flow
	if err := json.Unmarshal(raw, &flow); err != nil {
		t.Fatal(err)
	}
	return &flow
}

func TestSistemuinoAgentFlowIsValidAndKeepsCommercialConversation(t *testing.T) {
	flow := loadSistemuinoAgentFlow(t)
	if err := Validate(flow); err != nil {
		t.Fatalf("flujo Sistemuino inválido: %v", err)
	}
	result, err := Advance(flow, nil, "necesito una tienda", Deps{
		InputType: "text",
		Agent: func(AgentRequest) (string, string, error) {
			return "Cuéntame qué productos venderás.", "conversar", nil
		},
	})
	if err != nil || result.State == nil || result.State.NodeID != "n_espera" || len(result.Sends) != 1 {
		t.Fatalf("conversación comercial: result=%+v err=%v", result, err)
	}
}

func TestSistemuinoCanRequestClassifyAndSavePayment(t *testing.T) {
	flow := loadSistemuinoAgentFlow(t)
	asked, err := Advance(flow, nil, "ya pagué la factura", Deps{
		InputType: "text",
		Agent: func(AgentRequest) (string, string, error) {
			return "Envíame una captura completa del comprobante; quedará pendiente de validación.", "cobrar", nil
		},
	})
	if err != nil || asked.State == nil || asked.State.NodeID != "n_payment_wait" || len(asked.Sends) != 1 {
		t.Fatalf("solicitud de captura: result=%+v err=%v", asked, err)
	}
	wrongFormat, err := Advance(flow, asked.State, "aún no tengo la captura", Deps{InputType: "text"})
	if err != nil || wrongFormat.State == nil || wrongFormat.State.NodeID != "n_espera" || len(wrongFormat.Sends) != 1 {
		t.Fatalf("el Router debe enviar formatos no imagen a la espera conversacional configurada: %+v err=%v", wrongFormat, err)
	}

	var calls []string
	registered, err := Advance(flow, asked.State, "", Deps{
		InputType: "image", MediaID: "media-payment", WaID: "wamid-payment",
		AgentStructured: func(request AgentRequest) (AgentResult, error) {
			if request.NodeID != "n_classify_payment" || !containsInputType(request.Accepts, "image") || !request.Silent {
				t.Fatalf("contrato visual inesperado: %+v", request)
			}
			return AgentResult{Branch: "valid", Data: map[string]any{
				"provider": "yape", "amount": 120.0, "currency": "PEN",
				"occurredAt": "2026-08-02T10:30:00-05:00", "operationCode": "00123",
				"recipient": "Sistemuino", "confidence": 0.98,
			}}, nil
		},
		Tool: func(ref string, args map[string]string, vars map[string]string) (string, error) {
			calls = append(calls, ref)
			if vars["payment_receipt"] != "media-payment" {
				t.Fatalf("media no guardada por wait: %+v", vars)
			}
			if ref != "data_mutate" || args["object"] != "cobros" || args["operation"] != "create" ||
				args["field.estado"] != "valid" || args["field.proveedor"] != "yape" ||
				args["field.monto"] != "120" || args["field.operacion"] != "00123" ||
				args["field.destinatario"] != "Sistemuino" || args["field.confianza"] != "0.98" {
				t.Fatalf("registro no recibió la salida estructurada: ref=%s args=%+v", ref, args)
			}
			return `{"recordId":"payment-1","objectKey":"cobros","operation":"create","created":true,"idempotent":false,"data":{"estado":"valid"}}`, nil
		},
	})
	if err != nil || len(calls) != 1 || calls[0] != "data_mutate" ||
		!registered.Done || registered.Handoff || len(registered.Sends) != 1 {
		t.Fatalf("registro: calls=%v result=%+v err=%v", calls, registered, err)
	}
}

func TestSistemuinoDirectImageRoutesToReviewOrRetry(t *testing.T) {
	flow := loadSistemuinoAgentFlow(t)
	review, err := Advance(flow, nil, "", Deps{
		InputType: "image", WaID: "wamid-review",
		AgentStructured: func(AgentRequest) (AgentResult, error) {
			return AgentResult{Branch: "needs_review", Data: map[string]any{
				"provider": "plin", "amount": 120.0, "confidence": 0.6,
			}}, nil
		},
		Tool: func(ref string, args map[string]string, _ map[string]string) (string, error) {
			if ref != "data_mutate" || args["field.estado"] != "needs_review" || args["field.proveedor"] != "plin" {
				t.Fatalf("registro de revisión inesperado: ref=%s args=%+v", ref, args)
			}
			return `{"recordId":"payment-2","objectKey":"cobros","operation":"create","created":true,"idempotent":false,"data":{"estado":"needs_review"}}`, nil
		},
	})
	if err != nil || !review.Done || !review.Handoff || len(review.Sends) != 1 {
		t.Fatalf("revisión directa: result=%+v err=%v", review, err)
	}

	retry, err := Advance(flow, nil, "", Deps{
		InputType: "image",
		AgentStructured: func(AgentRequest) (AgentResult, error) {
			return AgentResult{Branch: "unreadable"}, nil
		},
		Tool: func(ref string, _ map[string]string, _ map[string]string) (string, error) {
			t.Fatalf("una imagen ilegible no debe registrar con %s", ref)
			return "", nil
		},
	})
	if err != nil || retry.State == nil || retry.State.NodeID != "n_espera" || len(retry.Sends) != 1 {
		t.Fatalf("reintento: result=%+v err=%v", retry, err)
	}
}
