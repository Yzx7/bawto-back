package engine

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
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
			// El wait ya no guarda un sobre bajo `payment_receipt`: nadie lo
			// consumía. Lo que sí debe llegar es el mensaje actual, del que sale
			// la clave de idempotencia del cobro.
			if vars["input.media.id"] != "media-payment" || vars["input.id"] != "wamid-payment" {
				t.Fatalf("el mensaje actual no llegó a la herramienta: %+v", vars)
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

// El contexto dinámico solo puede cambiar el prompt, nunca la ruta obligatoria
// del agente. Estas tres variantes cubren el caso sin perfil, el segmentado y
// el del segmento que ya no está activo, que es el que más fácil se cuela.
func TestSistemuinoResuelvePerfilYSegmentoAntesDelAgente(t *testing.T) {
	flow := loadSistemuinoAgentFlow(t)

	// Devuelve lo que cada lectura encuentra, en el orden en que el grafo las hace.
	conLecturas := func(respuestas ...string) (Deps, *[]string, *string) {
		var refs []string
		var contexto string
		index := 0
		deps := Deps{
			InputType: "text",
			Tool: func(ref string, args, _ map[string]string) (string, error) {
				refs = append(refs, ref+":"+args["object"])
				if index >= len(respuestas) {
					t.Fatalf("lectura inesperada %s", ref)
				}
				index++
				return respuestas[index-1], nil
			},
			AgentStructured: func(request AgentRequest) (AgentResult, error) {
				if request.NodeID == "n_agente" {
					contexto = request.Instruction
				}
				return AgentResult{Reply: "Cuéntame más.", Branch: "conversar"}, nil
			},
		}
		return deps, &refs, &contexto
	}

	t.Run("sin perfil el agente general atiende igual", func(t *testing.T) {
		deps, refs, contexto := conLecturas(`{"found":false,"count":0,"first":null,"records":[]}`)
		result, err := Advance(flow, nil, "hola", deps)
		if err != nil || result.State == nil || result.State.NodeID != "n_espera" || len(result.Sends) != 1 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if len(*refs) != 1 || (*refs)[0] != "data_query:perfiles_contacto" {
			t.Fatalf("no debe leer segmentos sin perfil: %v", *refs)
		}
		if strings.Contains(*contexto, "CONTEXTO INTERNO") {
			t.Fatal("un contacto sin perfil no puede recibir contexto de nadie")
		}
	})

	t.Run("perfil con segmento activo enriquece el prompt", func(t *testing.T) {
		deps, refs, contexto := conLecturas(
			`{"found":true,"count":1,"first":{"recordId":"p1","data":{"segmento_key":"ventas_b2b","contexto_personal":"Opera tres locales.","activo":true}},"records":[]}`,
			`{"found":true,"count":1,"first":{"recordId":"s1","data":{"clave":"ventas_b2b","nombre":"Ventas B2B","contexto":"Decide por costo total.","tono":"Sobrio.","restricciones":"Sin descuentos.","ruta":"contexto_especializado","activo":true}},"records":[]}`,
		)
		result, err := Advance(flow, nil, "hola", deps)
		if err != nil || result.State == nil || result.State.NodeID != "n_espera" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if len(*refs) != 2 || (*refs)[1] != "data_query:segmentos" {
			t.Fatalf("lecturas=%v", *refs)
		}
		for _, esperado := range []string{"CONTEXTO INTERNO", "Ventas B2B", "Decide por costo total.", "Opera tres locales."} {
			if !strings.Contains(*contexto, esperado) {
				t.Fatalf("falta %q en la instrucción", esperado)
			}
		}
		// La tabla entrega una clave semántica; jamás un id de nodo.
		if strings.Contains(*contexto, "n_agente") {
			t.Fatal("el contexto no puede contener ids del grafo")
		}
	})

	t.Run("segmento inactivo no enriquece", func(t *testing.T) {
		// El segmento llega encontrado y apagado a propósito: es lo que produce un
		// bloque al que se le quite el filtro `activo`. La defensa que se prueba
		// aquí es la del Router, que es la que queda si la otra desaparece.
		deps, refs, contexto := conLecturas(
			`{"found":true,"count":1,"first":{"recordId":"p1","data":{"segmento_key":"piloto_pausado","contexto_personal":"x","activo":true}},"records":[]}`,
			`{"found":true,"count":1,"first":{"recordId":"s3","data":{"clave":"piloto_pausado","nombre":"Piloto","contexto":"No debe llegar.","ruta":"contexto_especializado","activo":false}},"records":[]}`,
		)
		result, err := Advance(flow, nil, "hola", deps)
		if err != nil || result.State == nil || result.State.NodeID != "n_espera" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if len(*refs) != 2 {
			t.Fatalf("lecturas=%v", *refs)
		}
		if strings.Contains(*contexto, "CONTEXTO INTERNO") {
			t.Fatal("un segmento inactivo no puede enriquecer el prompt")
		}
	})

	t.Run("la ruta asesor deriva sin pasar por el agente", func(t *testing.T) {
		deps, _, _ := conLecturas(
			`{"found":true,"count":1,"first":{"recordId":"p1","data":{"segmento_key":"soporte_prioritario","activo":true}},"records":[]}`,
			`{"found":true,"count":1,"first":{"recordId":"s2","data":{"clave":"soporte_prioritario","ruta":"asesor","activo":true}},"records":[]}`,
		)
		deps.AgentStructured = func(AgentRequest) (AgentResult, error) {
			t.Fatal("la ruta asesor no debe llamar al agente")
			return AgentResult{}, nil
		}
		result, err := Advance(flow, nil, "hola", deps)
		if err != nil || !result.Handoff || !result.Done {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})

	t.Run("una lectura fallida no deja al cliente sin respuesta", func(t *testing.T) {
		result, err := Advance(flow, nil, "hola", Deps{
			InputType: "text",
			Tool: func(string, map[string]string, map[string]string) (string, error) {
				return "", errors.New("el objeto no existe")
			},
			AgentStructured: func(AgentRequest) (AgentResult, error) {
				return AgentResult{Reply: "Cuéntame más.", Branch: "conversar"}, nil
			},
		})
		if err != nil || len(result.Sends) != 1 || result.State == nil {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})
}
