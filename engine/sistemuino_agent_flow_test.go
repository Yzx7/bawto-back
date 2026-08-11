package engine

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// El fixture es una copia del borrador vivo exportado de PostgreSQL.
//
// Estuvo congelado once nodos por detrás mientras el flujo publicado ganaba el
// orquestador, los especialistas y el circuito de métodos de pago: las pruebas
// seguían verdes contra un grafo que ya no existía en producción. Al
// refrescarlo (2026-08-11) cinco de ellas se pusieron rojas de golpe. Si vuelve
// a divergir, esto deja de probar nada, así que **el fixture se refresca en el
// mismo cambio en que se toca el borrador**.
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

// enrutar despacha por nodo. El grafo tiene un orquestador y varios
// especialistas, y cada uno tiene sus propias ramas: un stub que devolviera
// siempre la misma haría que el motor cayera a una arista cualquiera y la
// prueba mediría el azar.
func enrutar(t *testing.T, ramas map[string]string) func(AgentRequest) (AgentResult, error) {
	t.Helper()
	return func(request AgentRequest) (AgentResult, error) {
		rama, ok := ramas[request.NodeID]
		if !ok {
			t.Fatalf("el grafo llamó a un agente inesperado: %s", request.NodeID)
		}
		return AgentResult{Reply: "…", Branch: rama}, nil
	}
}

func TestSistemuinoAgentFlowIsValidAndKeepsCommercialConversation(t *testing.T) {
	flow := loadSistemuinoAgentFlow(t)
	if err := Validate(flow); err != nil {
		t.Fatalf("flujo Sistemuino inválido: %v", err)
	}
	var lecturas []string
	result, err := Advance(flow, nil, "necesito una tienda online", Deps{
		InputType: "text",
		Tool: func(ref string, args, _ map[string]string) (string, error) {
			lecturas = append(lecturas, args["object"])
			return `{"found":true,"count":1,"first":{"recordId":"s1","data":{"nombre":"Ecommerce"}},"records":[]}`, nil
		},
		AgentStructured: enrutar(t, map[string]string{
			"n_agente":              "servicios",
			"n_services_specialist": "conversar",
		}),
	})
	if err != nil || result.State == nil || result.State.NodeID != "n_espera" || len(result.Sends) != 1 {
		t.Fatalf("conversación comercial: result=%+v err=%v", result, err)
	}
	// El especialista no responde de memoria: el catálogo se lee antes.
	if len(lecturas) == 0 || lecturas[len(lecturas)-1] != "servicios" {
		t.Fatalf("el especialista debe recibir el catálogo leído: %v", lecturas)
	}
}

func TestSistemuinoCanRequestClassifyAndSavePayment(t *testing.T) {
	flow := loadSistemuinoAgentFlow(t)

	asked, err := Advance(flow, nil, "ya pagué la factura", Deps{
		InputType: "text",
		Tool: func(ref string, _, _ map[string]string) (string, error) {
			if ref == "payment_methods_render" {
				return `{"found":true,"count":1,"message":"Paga a la cuenta X y envía tu comprobante."}`, nil
			}
			return `{"found":false,"count":0,"first":null,"records":[]}`, nil
		},
		AgentStructured: enrutar(t, map[string]string{
			"n_agente":             "pago",
			"n_payment_specialist": "cobrar",
		}),
	})
	if err != nil || asked.State == nil || asked.State.NodeID != "n_payment_wait" {
		t.Fatalf("solicitud de captura: result=%+v err=%v", asked, err)
	}
	// El número de cuenta lo compone el backend, no la IA: el mensaje enviado es
	// exactamente el que devolvió la herramienta.
	if len(asked.Sends) == 0 || !strings.Contains(asked.Sends[len(asked.Sends)-1], "Paga a la cuenta X") {
		t.Fatalf("las instrucciones exactas no llegaron: %v", asked.Sends)
	}

	wrongFormat, err := Advance(flow, asked.State, "aún no tengo la captura", Deps{InputType: "text"})
	if err != nil || wrongFormat.State == nil || wrongFormat.State.NodeID != "n_espera" || len(wrongFormat.Sends) != 1 {
		t.Fatalf("el Router debe enviar formatos no imagen a la espera conversacional: %+v err=%v", wrongFormat, err)
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
			// El wait no guarda un sobre: lo que debe llegar es el mensaje actual,
			// del que sale la clave de idempotencia del cobro.
			if vars["input.media.id"] != "media-payment" || vars["input.id"] != "wamid-payment" {
				t.Fatalf("el mensaje actual no llegó a la herramienta: %+v", vars)
			}
			if ref != "data_mutate" || args["object"] != "cobros" || args["operation"] != "create" ||
				args["field.proveedor"] != "yape" || args["field.monto"] != "120" ||
				args["field.operacion"] != "00123" || args["field.destinatario"] != "Sistemuino" ||
				args["field.confianza"] != "0.98" {
				t.Fatalf("registro no recibió la salida estructurada: ref=%s args=%+v", ref, args)
			}
			return `{"recordId":"payment-1","objectKey":"cobros","operation":"create","created":true,"idempotent":false,"data":{"estado":"aceptado"}}`, nil
		},
	})
	if err != nil || len(calls) != 1 || calls[0] != "data_mutate" || !registered.Done || len(registered.Sends) != 1 {
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
			if ref != "data_mutate" || args["field.proveedor"] != "plin" {
				t.Fatalf("registro de revisión inesperado: ref=%s args=%+v", ref, args)
			}
			return `{"recordId":"payment-2","objectKey":"cobros","operation":"create","created":true,"idempotent":false,"data":{"estado":"aceptado"}}`, nil
		},
	})
	// Una imagen suelta registra el cobro y termina: sin datos de venta no hay
	// suscripción que activar, así que la condición cae a la rama de revisión.
	if err != nil || !review.Done || len(review.Sends) != 1 {
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
// del agente. Estas variantes cubren el caso sin perfil, el segmentado y el del
// segmento que ya no está activo, que es el que más fácil se cuela.
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
				// El contexto de la organización lo interpola el **especialista**, no
				// el orquestador: `{contexto_organizacion}` no aparece en n_agente.
				// La prueba lo miraba en el sitio equivocado desde que el flujo se
				// dividió en orquestador y especialistas.
				if request.NodeID != "n_agente" {
					contexto = request.Instruction
				}
				if request.NodeID == "n_agente" {
					return AgentResult{Branch: "servicios"}, nil
				}
				return AgentResult{Reply: "Cuéntame más.", Branch: "conversar"}, nil
			},
		}
		return deps, &refs, &contexto
	}

	t.Run("sin perfil el agente general atiende igual", func(t *testing.T) {
		deps, refs, contexto := conLecturas(
			`{"found":false,"count":0,"first":null,"records":[]}`,
			`{"found":true,"count":1,"first":{"recordId":"s1","data":{"nombre":"Ecommerce"}},"records":[]}`,
		)
		result, err := Advance(flow, nil, "hola", deps)
		if err != nil || result.State == nil || result.State.NodeID != "n_espera" || len(result.Sends) != 1 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if (*refs)[0] != "data_query:perfiles_contacto" {
			t.Fatalf("la primera lectura debe ser el perfil: %v", *refs)
		}
		for _, ref := range *refs {
			if ref == "data_query:segmentos" {
				t.Fatalf("no debe leer segmentos sin perfil: %v", *refs)
			}
		}
		if strings.Contains(*contexto, "CONTEXTO INTERNO") {
			t.Fatal("un contacto sin perfil no puede recibir contexto de nadie")
		}
	})

	t.Run("perfil con segmento activo enriquece el prompt", func(t *testing.T) {
		deps, refs, contexto := conLecturas(
			`{"found":true,"count":1,"first":{"recordId":"p1","data":{"segmento_key":"ventas_b2b","contexto_personal":"Opera tres locales.","activo":true}},"records":[]}`,
			`{"found":true,"count":1,"first":{"recordId":"s1","data":{"clave":"ventas_b2b","nombre":"Ventas B2B","contexto":"Decide por costo total.","tono":"Sobrio.","restricciones":"Sin descuentos.","ruta":"contexto_especializado","activo":true}},"records":[]}`,
			`{"found":true,"count":1,"first":{"recordId":"c1","data":{"nombre":"Ecommerce"}},"records":[]}`,
		)
		result, err := Advance(flow, nil, "hola", deps)
		if err != nil || result.State == nil || result.State.NodeID != "n_espera" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if len(*refs) < 2 || (*refs)[1] != "data_query:segmentos" {
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
			`{"found":true,"count":1,"first":{"recordId":"c1","data":{"nombre":"Ecommerce"}},"records":[]}`,
		)
		result, err := Advance(flow, nil, "hola", deps)
		if err != nil || result.State == nil || result.State.NodeID != "n_espera" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if len(*refs) < 2 {
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
				return AgentResult{Reply: "Cuéntame más.", Branch: "aclarar"}, nil
			},
		})
		if err != nil || len(result.Sends) != 1 || result.State == nil {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})
}

// El Router «CTX obtained» que el dueño añadió antes de publicar corta la
// relectura cuando el segmento ya está en el estado de la conversación: ahorra
// dos consultas por turno en un chat ya resuelto.
//
// El precio es que el contexto queda congelado mientras dure la conversación.
// Las variables viven en `state.Vars` y sobreviven al wait, así que editar
// `segmentos.contexto` en el panel no se nota hasta que el chat termina o expira
// a las 24 h. Es una decisión visible en el grafo; esta prueba la fija para que
// no cambie por accidente y para que quede escrito cuál es el intercambio.
func TestSistemuinoNoRelaeElSegmentoYaResuelto(t *testing.T) {
	flow := loadSistemuinoAgentFlow(t)

	var instruccion string
	deps := Deps{
		InputType: "text",
		Tool: func(ref string, args, _ map[string]string) (string, error) {
			t.Fatalf("no debe releer %s sobre %q: el segmento ya está en el estado", ref, args["object"])
			return "", nil
		},
		AgentStructured: func(request AgentRequest) (AgentResult, error) {
			if request.NodeID == "n_agente" {
				return AgentResult{Branch: "pago"}, nil
			}
			instruccion = request.Instruction
			return AgentResult{Reply: "Sigo.", Branch: "conversar"}, nil
		},
	}
	estado := &State{NodeID: "n_espera", Vars: map[string]string{
		"segmento.found":                      "true",
		"segmento.first.data.activo":          "true",
		"segmento.first.data.ruta":            "contexto_especializado",
		"segmento.first.data.nombre":          "Ventas B2B",
		"segmento.first.data.contexto":        "Decide por costo total.",
		"segmento.first.data.tono":            "Sobrio.",
		"segmento.first.data.restricciones":   "Sin descuentos.",
		"perfil.first.data.contexto_personal": "Opera tres locales.",
	}}
	result, err := Advance(flow, estado, "otra pregunta", deps)
	if err != nil || result.State == nil || len(result.Sends) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !strings.Contains(instruccion, "Ventas B2B") {
		t.Fatal("el contexto en memoria debe seguir llegando al agente")
	}

	// Un segmento que se apagó entre turnos sí vuelve al agente general, porque
	// el Router comprueba `activo` sobre el valor cacheado.
	estado.Vars["segmento.first.data.activo"] = "false"
	var recargas []string
	deps.Tool = func(ref string, args, _ map[string]string) (string, error) {
		recargas = append(recargas, args["object"])
		return `{"found":false,"count":0,"first":null,"records":[]}`, nil
	}
	if _, err = Advance(flow, estado, "otra más", deps); err != nil {
		t.Fatal(err)
	}
	if len(recargas) == 0 {
		t.Fatal("con el segmento apagado debe volver a leer el perfil")
	}
	if strings.Contains(instruccion, "CONTEXTO INTERNO") {
		t.Fatal("un segmento apagado no puede seguir enriqueciendo")
	}
}

// La rama comercial de la tienda, de punta a punta: el orquestador entrega el
// tema, el especialista consulta el catálogo y confirma los tres datos, y el
// grafo —no el modelo— crea el pedido y abre el cobro.
func TestSistemuinoVendeUnProductoDelCatalogo(t *testing.T) {
	flow := loadSistemuinoAgentFlow(t)

	var llamadas []string
	var argsPedido map[string]string
	deps := Deps{
		InputType: "text", WaID: "wamid-compra",
		AgentStructured: func(request AgentRequest) (AgentResult, error) {
			switch request.NodeID {
			case "n_agente":
				return AgentResult{Branch: "productos"}, nil
			case "n_products_specialist":
				// El especialista es el único agente del flujo con herramientas.
				if len(request.Tools) != 2 {
					t.Fatalf("el especialista debe poder consultar el catálogo: %+v", request.Tools)
				}
				return AgentResult{Reply: "Perfecto, lo registro.", Branch: "comprar", Data: map[string]any{
					"productId": "51", "cantidad": 2.0, "correo": "ana@example.com",
				}}, nil
			}
			t.Fatalf("agente inesperado: %s", request.NodeID)
			return AgentResult{}, nil
		},
		Tool: func(ref string, args, _ map[string]string) (string, error) {
			llamadas = append(llamadas, ref)
			switch ref {
			case "data_query":
				// El grafo resuelve el perfil del contacto antes de cualquier rama.
				return `{"found":false,"count":0,"first":null,"records":[]}`, nil
			case "order_create":
				argsPedido = args
				return `{"orderId":11,"orderNumber":"ORD-1","status":"pending","total":17.8,"currency":"PEN","itemCount":1,"summary":"2 × Sensor PIR (S/ 17.80)"}`, nil
			case "payment_intent_create":
				return `{"paymentId":2,"orderId":11,"status":"pending","amount":17.8,"currency":"PEN","message":"Paga S/ 17.80 al Yape 999."}`, nil
			}
			t.Fatalf("herramienta inesperada: %s", ref)
			return "", nil
		},
	}

	result, err := Advance(flow, nil, "quiero dos sensores de movimiento", deps)
	if err != nil {
		t.Fatalf("venta: %v", err)
	}
	if len(llamadas) < 2 || llamadas[len(llamadas)-2] != "order_create" ||
		llamadas[len(llamadas)-1] != "payment_intent_create" {
		t.Fatalf("secuencia inesperada: %v", llamadas)
	}
	// El precio no viaja: lo calcula la tienda. La idempotencia sale del mensaje.
	if argsPedido["item.1.productId"] != "51" || argsPedido["item.1.quantity"] != "2" ||
		argsPedido["customerEmail"] != "ana@example.com" ||
		argsPedido["idempotencyKey"] != "message:wamid-compra" {
		t.Fatalf("argumentos del pedido: %+v", argsPedido)
	}
	// Queda esperando el comprobante, no en el bucle general: el contexto del
	// pedido tiene que sobrevivir al turno.
	if result.State == nil || result.State.NodeID != "n_store_receipt_wait" {
		t.Fatalf("debe esperar el comprobante del pedido: %+v", result)
	}
	if len(result.Sends) == 0 || !strings.Contains(result.Sends[len(result.Sends)-1], "Paga S/ 17.80 al Yape 999.") {
		t.Fatalf("las instrucciones exactas de la tienda no llegaron: %v", result.Sends)
	}

	// El comprobante declara la operación contra la tienda.
	var declaracion map[string]string
	pago, err := Advance(flow, result.State, "", Deps{
		InputType: "image", MediaID: "media-1", WaID: "wamid-comprobante",
		AgentStructured: func(request AgentRequest) (AgentResult, error) {
			if request.NodeID != "n_store_receipt" || !request.Silent {
				t.Fatalf("agente visual de la tienda inesperado: %+v", request)
			}
			return AgentResult{Branch: "valid", Data: map[string]any{"operationCode": "00987"}}, nil
		},
		Tool: func(ref string, args, _ map[string]string) (string, error) {
			if ref != "payment_submit" {
				t.Fatalf("herramienta inesperada: %s", ref)
			}
			declaracion = args
			return `{"paymentId":2,"orderId":11,"status":"submitted","reference":"00987"}`, nil
		},
	})
	if err != nil || !pago.Done || len(pago.Sends) != 1 {
		t.Fatalf("declaración: %+v err=%v", pago, err)
	}
	if declaracion["reference"] != "00987" || declaracion["paymentId"] != "2" {
		t.Fatalf("la referencia del comprobante no llegó: %+v", declaracion)
	}
}

// Sin los tres datos confirmados no se crea medio pedido: el Router es la
// segunda barrera detrás de la instrucción del especialista.
func TestSistemuinoNoCreaPedidoSinCorreo(t *testing.T) {
	flow := loadSistemuinoAgentFlow(t)
	result, err := Advance(flow, nil, "quiero ese sensor", Deps{
		InputType: "text",
		AgentStructured: func(request AgentRequest) (AgentResult, error) {
			if request.NodeID == "n_agente" {
				return AgentResult{Branch: "productos"}, nil
			}
			return AgentResult{Reply: "¿Me das tu correo?", Branch: "comprar", Data: map[string]any{
				"productId": "51", "cantidad": 1.0,
			}}, nil
		},
		Tool: func(ref string, _, _ map[string]string) (string, error) {
			if ref == "data_query" {
				return `{"found":false,"count":0,"first":null,"records":[]}`, nil
			}
			t.Fatalf("no debe llamar a %s sin el correo del comprador", ref)
			return "", nil
		},
	})
	if err != nil || result.State == nil || result.State.NodeID != "n_espera" {
		t.Fatalf("debe volver al bucle a pedir el dato: %+v err=%v", result, err)
	}
}

// El fallo de la tienda llega al cliente con su causa. Antes el motor descartaba
// el texto y solo quedaba la rama, así que «quedan 2» se perdía.
func TestSistemuinoCuentaPorQueFallaElPedido(t *testing.T) {
	flow := loadSistemuinoAgentFlow(t)
	result, err := Advance(flow, nil, "quiero cinco", Deps{
		InputType: "text", WaID: "wamid-stock",
		AgentStructured: func(request AgentRequest) (AgentResult, error) {
			if request.NodeID == "n_agente" {
				return AgentResult{Branch: "productos"}, nil
			}
			return AgentResult{Branch: "comprar", Data: map[string]any{
				"productId": "51", "cantidad": 5.0, "correo": "ana@example.com",
			}}, nil
		},
		Tool: func(ref string, _, _ map[string]string) (string, error) {
			if ref == "data_query" {
				return `{"found":false,"count":0,"first":null,"records":[]}`, nil
			}
			return "", errors.New("stock insuficiente para 'Sensor PIR' (disponible: 2, solicitado: 5)")
		},
	})
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(result.Sends) == 0 || !strings.Contains(result.Sends[len(result.Sends)-1], "disponible: 2") {
		t.Fatalf("el cliente debe enterarse de por qué falló: %v", result.Sends)
	}
}
