package engine

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func TestAdvanceMessageFlow(t *testing.T) {
	flow := &Flow{
		Trigger: Trigger{Type: "message", Match: "any"},
		Nodes: []Node{
			{ID: "s1", Kind: "send", Body: "Hola, dijiste: {input}"},
			{ID: "w1", Kind: "wait", SaveAs: "resp"},
			{ID: "c1", Kind: "condition", Expression: "resp == si"},
			{ID: "yes", Kind: "send", Body: "¡Sí!"},
			{ID: "no", Kind: "send", Body: "No."},
		},
		Edges: []Edge{
			{Source: "trigger", Target: "s1"},
			{Source: "s1", Target: "w1"},
			{Source: "w1", Target: "c1"},
			{Source: "c1", SourceHandle: "true", Target: "yes"},
			{Source: "c1", SourceHandle: "false", Target: "no"},
		},
	}

	// Primer mensaje: envía saludo (con interpolación) y pausa en wait.
	r, err := Advance(flow, nil, "hola", Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Sends) != 1 || r.Sends[0] != "Hola, dijiste: hola" {
		t.Fatalf("sends1: %v", r.Sends)
	}
	if r.State == nil || r.State.NodeID != "w1" || r.Done {
		t.Fatalf("estado tras wait: %+v done=%v", r.State, r.Done)
	}

	// Reanuda con "si" → rama true.
	r2, _ := Advance(flow, r.State, "si", Deps{})
	if len(r2.Sends) != 1 || r2.Sends[0] != "¡Sí!" || !r2.Done {
		t.Fatalf("sends2: %v done=%v", r2.Sends, r2.Done)
	}

	// Reanuda con otra cosa → rama false.
	r3, _ := Advance(flow, r.State, "nope", Deps{})
	if len(r3.Sends) != 1 || r3.Sends[0] != "No." {
		t.Fatalf("sends3: %v", r3.Sends)
	}
}

func TestAdvanceAgentProjectsDeclaredStructuredOutput(t *testing.T) {
	flow := &Flow{
		Trigger: Trigger{Type: "message", Match: "any"},
		Nodes: []Node{
			{
				ID: "vision", Kind: "agent", AgentRole: "specialist", Instruction: "Extrae el comprobante", Outputs: []string{"comprobante", "revision"},
				SaveAs: "pago", OutputFields: []AgentOutputField{
					{Key: "provider", Type: "string"},
					{Key: "amount", Type: "number"},
				},
			},
			{ID: "ok", Kind: "send", Body: "{pago.branch}: {pago.provider} S/{pago.amount} {pago.ignored}"},
		},
		Edges: []Edge{
			{Source: "trigger", Target: "vision"},
			{Source: "vision", SourceHandle: "comprobante", Target: "ok"},
		},
	}

	result, err := Advance(flow, nil, "captura", Deps{
		InputType: "image",
		AgentStructured: func(request AgentRequest) (AgentResult, error) {
			if request.AgentRole != "specialist" {
				t.Fatalf("rol no propagado: %q", request.AgentRole)
			}
			if len(request.OutputFields) != 2 || request.OutputFields[1].Key != "amount" {
				t.Fatalf("esquema no propagado: %#v", request.OutputFields)
			}
			return AgentResult{
				Branch: "comprobante",
				Data:   map[string]any{"provider": "yape", "amount": 120.5, "ignored": "no declarado"},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sends) != 1 || result.Sends[0] != "comprobante: yape S/120.5 {pago.ignored}" {
		t.Fatalf("variables proyectadas=%#v", result.Sends)
	}
}

func TestWaitRequiresExpectedInputAndPreservesMedia(t *testing.T) {
	flow := &Flow{Trigger: Trigger{Type: "message"}, Nodes: []Node{
		{ID: "wait", Kind: "wait", Expect: "image", SaveAs: "receipt", TimeoutHours: 2},
		{ID: "tool", Kind: "tool", ToolRef: "record"},
		{ID: "ok", Kind: "action", Action: "end"},
		{ID: "bad", Kind: "action", Action: "end"},
	}, Edges: []Edge{{Source: "trigger", Target: "wait"}, {Source: "wait", Target: "tool"}, {Source: "tool", SourceHandle: "ok", Target: "ok"}, {Source: "tool", SourceHandle: "error", Target: "bad"}}}
	first, err := Advance(flow, nil, "", Deps{InputType: "text"})
	if err != nil || first.State == nil {
		t.Fatalf("inicio: state=%+v err=%v", first.State, err)
	}
	rejected, err := Advance(flow, first.State, "no es foto", Deps{InputType: "text"})
	if err != nil || rejected.State == nil || len(rejected.Sends) != 1 {
		t.Fatalf("entrada incorrecta: %+v err=%v", rejected, err)
	}
	called := false
	accepted, err := Advance(flow, first.State, "caption", Deps{InputType: "image", MediaID: "media-1", WaID: "wamid-1", Tool: func(_ string, _ map[string]string, vars map[string]string) (string, error) {
		called = vars["receipt"] == "media-1" && vars["receipt_media_id"] == "media-1" && vars["input_wa_id"] == "wamid-1"
		return "ok", nil
	}})
	if err != nil || !called || !accepted.Done {
		t.Fatalf("imagen: called=%v result=%+v err=%v", called, accepted, err)
	}
}

func TestTriggerMatchesKeywords(t *testing.T) {
	trigger := Trigger{Type: "message", Match: "keyword", Keywords: []string{"soporte", "asesor"}}
	if !TriggerMatches(trigger, "Necesito SOPORTE técnico") || TriggerMatches(trigger, "hola") {
		t.Fatal("keyword matching incorrecto")
	}
}

func TestTriggerMatchesInputType(t *testing.T) {
	trigger := Trigger{Type: "message", Match: "any", Accepts: []string{"image", "audio"}}
	if !TriggerMatchesInput(trigger, "caption", "image") || !TriggerMatchesInput(trigger, "", "audio") {
		t.Fatal("los tipos admitidos deberían iniciar el flujo")
	}
	if TriggerMatchesInput(trigger, "hola", "text") {
		t.Fatal("texto no debería iniciar un flujo que solo acepta media")
	}
}

func TestTypedTriggerRoutesByContentType(t *testing.T) {
	flow := &Flow{
		Trigger: Trigger{Type: "message", Match: "any", Accepts: []string{"text", "image"}, RouteBy: "content_type"},
		Nodes: []Node{
			{ID: "text", Kind: "send", Body: "T"}, {ID: "image", Kind: "send", Body: "I"},
			{ID: "end-text", Kind: "action", Action: "end"}, {ID: "end-image", Kind: "action", Action: "end"},
		},
		Edges: []Edge{
			{Source: "trigger", SourceHandle: "text", Target: "text"},
			{Source: "trigger", SourceHandle: "image", Target: "image"},
			{Source: "text", Target: "end-text"}, {Source: "image", Target: "end-image"},
		},
	}
	textResult, err := Advance(flow, nil, "hola", Deps{InputType: "text"})
	if err != nil || len(textResult.Sends) != 1 || textResult.Sends[0] != "T" {
		t.Fatalf("ruta texto: %+v err=%v", textResult, err)
	}
	imageResult, err := Advance(flow, nil, "caption", Deps{InputType: "image", MediaID: "m1"})
	if err != nil || len(imageResult.Sends) != 1 || imageResult.Sends[0] != "I" {
		t.Fatalf("ruta imagen: %+v err=%v", imageResult, err)
	}
}

func TestWaitAcceptsMultipleInputTypes(t *testing.T) {
	flow := &Flow{
		Trigger: Trigger{Type: "message"},
		Nodes: []Node{
			{ID: "wait", Kind: "wait", Accepts: []string{"image", "document"}, SaveAs: "proof"},
			{ID: "image", Kind: "send", Body: "imagen"}, {ID: "document", Kind: "send", Body: "documento"},
		},
		Edges: []Edge{
			{Source: "trigger", Target: "wait"},
			{Source: "wait", SourceHandle: "image", Target: "image"},
			{Source: "wait", SourceHandle: "document", Target: "document"},
		},
	}
	waiting, err := Advance(flow, nil, "inicio", Deps{InputType: "text"})
	if err != nil || waiting.State == nil {
		t.Fatalf("inicio: %+v err=%v", waiting, err)
	}
	wrong, _ := Advance(flow, waiting.State, "texto", Deps{InputType: "text"})
	if wrong.State == nil || len(wrong.Sends) != 1 {
		t.Fatalf("formato incorrecto: %+v", wrong)
	}
	document, err := Advance(flow, waiting.State, "", Deps{InputType: "document", MediaID: "doc-1"})
	if err != nil || len(document.Sends) != 1 || document.Sends[0] != "documento" {
		t.Fatalf("documento: %+v err=%v", document, err)
	}
}

func TestWaitCapturesAnyNextEventAndSaveAsIsOptional(t *testing.T) {
	flow := &Flow{
		Trigger: Trigger{Type: "message"},
		Nodes: []Node{
			{ID: "wait", Kind: "wait", TimeoutHours: 24},
			{ID: "router", Kind: "router", Cases: []RouterCase{{ID: "image", Expression: `input.contentType == image`}}},
			{ID: "image", Kind: "send", Body: "imagen {input.media.id}"},
			{ID: "other", Kind: "send", Body: "otro {input.contentType}"},
		},
		Edges: []Edge{
			{Source: "trigger", Target: "wait"},
			{Source: "wait", Target: "router"},
			{Source: "router", SourceHandle: "image", Target: "image"},
			{Source: "router", SourceHandle: "default", Target: "other"},
		},
	}
	waiting, err := Advance(flow, nil, "inicio", Deps{InputType: "text"})
	if err != nil || waiting.State == nil {
		t.Fatalf("inicio: %+v err=%v", waiting, err)
	}
	result, err := Advance(flow, waiting.State, "captura", Deps{Input: InboundInput{
		ID: "wamid-2", ContentType: "image", MediaID: "media-2", Caption: "captura",
	}})
	if err != nil || len(result.Sends) != 1 || result.Sends[0] != "imagen media-2" {
		t.Fatalf("imagen: %+v err=%v", result, err)
	}
}

func TestWaitSaveAsProjectsStructuredEnvelope(t *testing.T) {
	flow := &Flow{
		Trigger: Trigger{Type: "message"},
		Nodes: []Node{
			{ID: "wait", Kind: "wait", SaveAs: "respuesta"},
			{ID: "send", Kind: "send", Body: "{respuesta.contentType}:{respuesta.media.id}:{respuesta.caption}"},
		},
		Edges: []Edge{{Source: "trigger", Target: "wait"}, {Source: "wait", Target: "send"}},
	}
	waiting, _ := Advance(flow, nil, "inicio", Deps{InputType: "text"})
	result, err := Advance(flow, waiting.State, "foto", Deps{Input: InboundInput{
		ContentType: "image", MediaID: "m-10", Caption: "foto",
	}})
	if err != nil || len(result.Sends) != 1 || result.Sends[0] != "image:m-10:foto" {
		t.Fatalf("sobre guardado: %+v err=%v", result, err)
	}
}

func TestToolArgumentsAreInterpolated(t *testing.T) {
	flow := &Flow{
		Trigger: Trigger{Type: "message"},
		Nodes: []Node{
			{ID: "tool", Kind: "tool", ToolRef: "data_mutate", Args: map[string]string{
				"field.message": "{input.text}", "idempotencyKey": "message:{input.id}",
			}},
			{ID: "ok", Kind: "action", Action: "end"},
			{ID: "error", Kind: "action", Action: "end"},
		},
		Edges: []Edge{
			{Source: "trigger", Target: "tool"},
			{Source: "tool", SourceHandle: "ok", Target: "ok"},
			{Source: "tool", SourceHandle: "error", Target: "error"},
		},
	}
	var got map[string]string
	_, err := Advance(flow, nil, "hola", Deps{Input: InboundInput{ID: "wamid-1", ContentType: "text"},
		Tool: func(_ string, args, _ map[string]string) (string, error) {
			got = args
			return `{}`, nil
		}})
	if err != nil || got["field.message"] != "hola" || got["idempotencyKey"] != "message:wamid-1" {
		t.Fatalf("args=%v err=%v", got, err)
	}
}

func TestBOTIFlowIsValid(t *testing.T) {
	raw, err := os.ReadFile("../db/flows/boti.json")
	if err != nil {
		t.Fatal(err)
	}
	var flow Flow
	if err = json.Unmarshal(raw, &flow); err != nil {
		t.Fatal(err)
	}
	if err = Validate(&flow); err != nil {
		t.Fatalf("BOTI inválido: %v", err)
	}
}

func TestBOTIPaymentAndSupportJourneys(t *testing.T) {
	raw, err := os.ReadFile("../db/flows/boti.json")
	if err != nil {
		t.Fatal(err)
	}
	var flow Flow
	if err = json.Unmarshal(raw, &flow); err != nil {
		t.Fatal(err)
	}
	agent := func(request AgentRequest) (string, string, error) {
		vars, outputs := request.Vars, request.Outputs
		if contains(outputs, "pago") {
			if vars["input"] == "1" {
				return "Registraré tu pago.", "pago", nil
			}
			if vars["input"] == "2 no tengo internet" {
				return "Revisemos tu conexión.", "soporte", nil
			}
			return "Elige una opción.", "menu", nil
		}
		if contains(outputs, "verificar") {
			return "Reinicia el router durante 30 segundos.", "verificar", nil
		}
		return "", "escalar", nil
	}

	payment, err := Advance(&flow, nil, "1", Deps{InputType: "text", Agent: agent})
	if err != nil || payment.State == nil || payment.State.NodeID != "n_espera_comprobante" {
		t.Fatalf("inicio pago: %+v err=%v", payment, err)
	}
	wrong, _ := Advance(&flow, payment.State, "texto", Deps{InputType: "text", Agent: agent})
	if wrong.State == nil || wrong.State.NodeID != "n_espera_comprobante" {
		t.Fatalf("pago aceptó texto: %+v", wrong)
	}
	paid, err := Advance(&flow, payment.State, "voucher", Deps{
		InputType: "image", MediaID: "media", WaID: "wamid", Agent: agent,
		AgentStructured: func(request AgentRequest) (AgentResult, error) {
			if request.NodeID != "n_clasifica_comprobante" {
				t.Fatalf("agente visual inesperado: %+v", request)
			}
			return AgentResult{Branch: "valid", Data: map[string]any{
				"provider": "yape", "amount": 50, "currency": "PEN",
				"occurredAt": "2026-08-02T10:30:00-05:00", "operationCode": "B-1",
				"recipient": "BOTI", "confidence": .98,
			}}, nil
		},
		Tool: func(ref string, args map[string]string, _ map[string]string) (string, error) {
			if ref != "data_mutate" || args["field.estado"] != "valid" {
				t.Fatalf("guardado inesperado: ref=%s args=%+v", ref, args)
			}
			return `{"recordId":"payment","objectKey":"cobros","operation":"create","created":true,"idempotent":false,"data":{"estado":"valid"}}`, nil
		},
	})
	if err != nil || !paid.Done || len(paid.Sends) == 0 || paid.Handoff {
		t.Fatalf("pago final: %+v err=%v", paid, err)
	}

	support, err := Advance(&flow, nil, "2 no tengo internet", Deps{InputType: "text", Agent: agent})
	if err != nil || support.State == nil || support.State.NodeID != "n_espera_soporte" {
		t.Fatalf("inicio soporte: %+v err=%v", support, err)
	}
	escalated, err := Advance(&flow, support.State, "no", Deps{InputType: "text", Agent: agent})
	if err != nil || !escalated.Done || !escalated.Handoff {
		t.Fatalf("escalamiento soporte: %+v err=%v", escalated, err)
	}
}

func contains(items []string, expected string) bool {
	for _, item := range items {
		if item == expected {
			return true
		}
	}
	return false
}

func TestAdvanceAgentBranch(t *testing.T) {
	flow := &Flow{
		Trigger: Trigger{Type: "message"},
		Nodes: []Node{
			{ID: "a1", Kind: "agent", Instruction: "valida {input}", Outputs: []string{"ok", "bad"}},
			{ID: "okn", Kind: "send", Body: "validado"},
			{ID: "badn", Kind: "send", Body: "rechazado"},
		},
		Edges: []Edge{
			{Source: "trigger", Target: "a1"},
			{Source: "a1", SourceHandle: "ok", Target: "okn"},
			{Source: "a1", SourceHandle: "bad", Target: "badn"},
		},
	}
	deps := Deps{
		Agent: func(request AgentRequest) (string, string, error) {
			nodeID, instr := request.NodeID, request.Instruction
			if nodeID != "a1" {
				t.Fatalf("node id inesperado: %q", nodeID)
			}
			if instr != "valida comprobante" {
				t.Fatalf("instrucción interpolada mal: %q", instr)
			}
			return "revisando…", "ok", nil
		},
	}
	r, err := Advance(flow, nil, "comprobante", deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Sends) != 2 || r.Sends[0] != "revisando…" || r.Sends[1] != "validado" || !r.Done {
		t.Fatalf("agent flow: %v done=%v", r.Sends, r.Done)
	}

	// Un agente clasificador (silent) elige la rama pero no le escribe al cliente:
	// si no, se duplica el mensaje con el `send` que sigue.
	flow.Nodes[0].Silent = true
	r, err = Advance(flow, nil, "comprobante", deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Sends) != 1 || r.Sends[0] != "validado" {
		t.Fatalf("agente silencioso: %v", r.Sends)
	}

	// Un orquestador puede preguntar solo en una rama y callar cuando ya pudo
	// entregar el turno a un especialista.
	flow.Nodes[0].Silent = false
	flow.Nodes[0].ReplyOn = []string{"bad"}
	r, err = Advance(flow, nil, "comprobante", deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Sends) != 1 || r.Sends[0] != "validado" {
		t.Fatalf("respuesta fuera de replyOn: %v", r.Sends)
	}
	flow.Nodes[0].ReplyOn = []string{"ok"}
	r, err = Advance(flow, nil, "comprobante", deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Sends) != 2 || r.Sends[0] != "revisando…" {
		t.Fatalf("respuesta dentro de replyOn: %v", r.Sends)
	}
}

func TestAgentHistoryPersistsAcrossLoopback(t *testing.T) {
	flow := &Flow{
		Trigger: Trigger{Type: "message"},
		Nodes: []Node{
			{ID: "agent", Kind: "agent", Instruction: "orienta", Outputs: []string{"continuar", "listo"}, ContextMode: "recent"},
			{ID: "wait", Kind: "wait", Expect: "text", SaveAs: "detalle", TimeoutHours: 24},
			{ID: "end", Kind: "action", Action: "end"},
		},
		Edges: []Edge{
			{Source: "trigger", Target: "agent"},
			{Source: "agent", SourceHandle: "continuar", Target: "wait"},
			{Source: "agent", SourceHandle: "listo", Target: "end"},
			{Source: "wait", Target: "agent"},
		},
	}

	calls := 0
	deps := Deps{InputType: "text"}
	deps.Agent = func(request AgentRequest) (string, string, error) {
		history := request.History
		calls++
		switch calls {
		case 1:
			if len(history) != 1 || history[0].Role != "user" || history[0].Content != "Tengo un negocio" {
				t.Fatalf("primer historial: %+v", history)
			}
			return "¿Qué deseas mejorar primero?", "continuar", nil
		case 2:
			want := []ChatMessage{
				{Role: "user", Content: "Tengo un negocio"},
				{Role: "assistant", Content: "¿Qué deseas mejorar primero?"},
				{Role: "user", Content: "Quiero vender por internet"},
			}
			if len(history) != len(want) {
				t.Fatalf("historial loopback: %+v", history)
			}
			for i := range want {
				if history[i] != want[i] {
					t.Fatalf("historial[%d]=%+v, want %+v", i, history[i], want[i])
				}
			}
			return "Una tienda en línea encaja con tu objetivo.", "listo", nil
		default:
			t.Fatalf("llamada inesperada: %d", calls)
			return "", "", nil
		}
	}

	first, err := Advance(flow, nil, "Tengo un negocio", deps)
	if err != nil || first.State == nil || first.State.NodeID != "wait" {
		t.Fatalf("primer turno: result=%+v err=%v", first, err)
	}
	if len(first.State.History) != 2 {
		t.Fatalf("estado sin historial: %+v", first.State)
	}

	second, err := Advance(flow, first.State, "Quiero vender por internet", deps)
	if err != nil || !second.Done || len(second.Sends) != 1 {
		t.Fatalf("segundo turno: result=%+v err=%v", second, err)
	}
}

func TestAgentWithoutContextReceivesEmptyHistory(t *testing.T) {
	flow := &Flow{
		Trigger: Trigger{Type: "message"},
		Nodes: []Node{
			{ID: "agent", Kind: "agent", Instruction: "clasifica", Outputs: []string{"listo"}},
			{ID: "end", Kind: "action", Action: "end"},
		},
		Edges: []Edge{
			{Source: "trigger", Target: "agent"},
			{Source: "agent", SourceHandle: "listo", Target: "end"},
		},
	}

	called := false
	deps := Deps{InputType: "text"}
	deps.Agent = func(request AgentRequest) (string, string, error) {
		vars, history := request.Vars, request.History
		called = true
		if vars["input"] != "mensaje actual" {
			t.Fatalf("input ausente: %+v", vars)
		}
		if len(history) != 0 {
			t.Fatalf("un agente sin contexto recibió historial: %+v", history)
		}
		return "", "listo", nil
	}

	result, err := Advance(flow, nil, "mensaje actual", deps)
	if err != nil || !result.Done || !called {
		t.Fatalf("result=%+v called=%v err=%v", result, called, err)
	}
}

func TestWrongWaitInputIsAddedToHistory(t *testing.T) {
	flow := &Flow{
		Trigger: Trigger{Type: "message"},
		Nodes: []Node{
			{ID: "wait", Kind: "wait", Expect: "image", TimeoutHours: 24},
			{ID: "agent", Kind: "agent", Instruction: "revisa", Outputs: []string{"fin"}, ContextMode: "recent"},
			{ID: "end", Kind: "action", Action: "end"},
		},
		Edges: []Edge{
			{Source: "trigger", Target: "wait"},
			{Source: "wait", Target: "agent"},
			{Source: "agent", SourceHandle: "fin", Target: "end"},
		},
	}
	first, err := Advance(flow, nil, "inicio", Deps{InputType: "text"})
	if err != nil || first.State == nil {
		t.Fatalf("inicio: result=%+v err=%v", first, err)
	}
	rejected, err := Advance(flow, first.State, "te mando texto", Deps{InputType: "text"})
	if err != nil || rejected.State == nil {
		t.Fatalf("rechazo: result=%+v err=%v", rejected, err)
	}
	history := rejected.State.History
	if len(history) != 3 || history[1].Content != "te mando texto" ||
		history[2].Content != "Necesito que envíes una imagen para continuar." {
		t.Fatalf("historial de rechazo: %+v", history)
	}
}

func TestFlowWithoutContextDoesNotPersistHistory(t *testing.T) {
	flow := &Flow{
		Trigger: Trigger{Type: "message"},
		Nodes:   []Node{{ID: "wait", Kind: "wait", Expect: "text", TimeoutHours: 24}},
		Edges:   []Edge{{Source: "trigger", Target: "wait"}},
	}

	result, err := Advance(flow, nil, "hola", Deps{InputType: "text"})
	if err != nil || result.State == nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(result.State.History) != 0 {
		t.Fatalf("un flujo sin memoria persistió historial: %+v", result.State.History)
	}
}

func TestSistemuinoDiscoveryUsesConversationalLoopback(t *testing.T) {
	raw, err := os.ReadFile("../db/flows/sistemuino.json")
	if err != nil {
		t.Fatal(err)
	}
	var flow Flow
	if err := json.Unmarshal(raw, &flow); err != nil {
		t.Fatal(err)
	}
	if err := Validate(&flow); err != nil {
		t.Fatalf("flujo inválido: %v", err)
	}

	routerCalls := 0
	deps := Deps{InputType: "text"}
	deps.Agent = func(request AgentRequest) (string, string, error) {
		outputs, history := request.Outputs, request.History
		if !contains(outputs, "conversar") {
			t.Fatalf("el orientador no expone conversar: %v", outputs)
		}
		routerCalls++
		switch routerCalls {
		case 1:
			if history[len(history)-1].Content != "Tengo un negocio" {
				t.Fatalf("inicio no llegó al orientador: %+v", history)
			}
			return "¿Qué te gustaría mejorar en tu negocio?", "conversar", nil
		case 2:
			if history[len(history)-1].Content != "Quiero mejorar mi negocio" {
				t.Fatalf("primer loop no llegó al orientador: %+v", history)
			}
			return "¿Buscas vender, cobrar o mejorar tu atención?", "conversar", nil
		case 3:
			foundQuestion := false
			for _, message := range history {
				if message.Role == "assistant" && message.Content == "¿Qué te gustaría mejorar en tu negocio?" {
					foundQuestion = true
				}
			}
			if !foundQuestion || history[len(history)-1].Content != "Quiero vender mis productos por internet" {
				t.Fatalf("el loopback perdió el historial: %+v", history)
			}
			return "Una tienda en línea encaja con lo que buscas.", "ecommerce", nil
		default:
			t.Fatalf("llamada inesperada al orientador: %d", routerCalls)
			return "", "", nil
		}
	}

	first, err := Advance(&flow, nil, "Tengo un negocio", deps)
	if err != nil || first.State == nil || first.State.NodeID != "n_discovery_wait" {
		t.Fatalf("inicio: result=%+v err=%v", first, err)
	}
	second, err := Advance(&flow, first.State, "Quiero mejorar mi negocio", deps)
	if err != nil || second.State == nil || second.State.NodeID != "n_discovery_wait" {
		t.Fatalf("primer loop: result=%+v err=%v", second, err)
	}
	third, err := Advance(&flow, second.State, "Quiero vender mis productos por internet", deps)
	if err != nil || third.State == nil || third.State.NodeID != "n_ecommerce_wait" {
		t.Fatalf("salida concreta: result=%+v err=%v", third, err)
	}
	if routerCalls != 3 {
		t.Fatalf("llamadas del orientador=%d", routerCalls)
	}
}

// Una opción elegida por el orientador va directo a un wait: el cliente recibe un
// solo mensaje por turno y quien lo atiende después es el agente especializado.
func TestSistemuinoOptionLeadsToWaitThenSpecialistAgent(t *testing.T) {
	raw, err := os.ReadFile("../db/flows/sistemuino.json")
	if err != nil {
		t.Fatal(err)
	}
	var flow Flow
	if err := json.Unmarshal(raw, &flow); err != nil {
		t.Fatal(err)
	}
	if err := Validate(&flow); err != nil {
		t.Fatalf("flujo inválido: %v", err)
	}

	var visited []string
	deps := Deps{InputType: "text"}
	deps.Agent = func(request AgentRequest) (string, string, error) {
		nodeID, outputs, silent := request.NodeID, request.Outputs, request.Silent
		visited = append(visited, nodeID)
		if silent {
			t.Fatalf("%s no debe ser silencioso: es el único mensaje del turno", nodeID)
		}
		switch nodeID {
		case "n_route":
			return "Hacemos sitios y aplicaciones web a medida. ¿Cuál es el objetivo del proyecto?", "web", nil
		case "n_web_agent":
			if !contains(outputs, "seguir") || !contains(outputs, "asesor") {
				t.Fatalf("el consultor web no expone seguir/asesor: %v", outputs)
			}
			if len(visited) == 2 {
				return "Entiendo, una web para captar clientes. ¿Tienes fecha de lanzamiento?", "seguir", nil
			}
			return "Perfecto, te paso con un especialista para el alcance.", "asesor", nil
		default:
			t.Fatalf("nodo IA inesperado: %s", nodeID)
			return "", "", nil
		}
	}

	first, err := Advance(&flow, nil, "Necesito una página web", deps)
	if err != nil || first.State == nil || first.State.NodeID != "n_web_wait" {
		t.Fatalf("elección de opción: result=%+v err=%v", first, err)
	}
	if len(first.Sends) != 1 {
		t.Fatalf("la opción envió %d mensajes: %+v", len(first.Sends), first.Sends)
	}

	second, err := Advance(&flow, first.State, "Para captar clientes", deps)
	if err != nil || second.State == nil || second.State.NodeID != "n_web_wait" {
		t.Fatalf("loopback del especialista: result=%+v err=%v", second, err)
	}
	if len(second.Sends) != 1 {
		t.Fatalf("el especialista envió %d mensajes: %+v", len(second.Sends), second.Sends)
	}
	if second.State.Vars["web_requirement"] != "Para captar clientes" {
		t.Fatalf("la espera no guardó el requerimiento: %+v", second.State.Vars)
	}

	third, err := Advance(&flow, second.State, "Sí, adelante", deps)
	if err != nil || !third.Done || !third.Handoff {
		t.Fatalf("derivación: result=%+v err=%v", third, err)
	}
	if len(third.Sends) != 1 {
		t.Fatalf("la derivación envió %d mensajes: %+v", len(third.Sends), third.Sends)
	}
	if len(visited) != 3 || visited[0] != "n_route" || visited[1] != "n_web_agent" || visited[2] != "n_web_agent" {
		t.Fatalf("recorrido de agentes inesperado: %v", visited)
	}
}

func TestAgentReceivesOnlyExplicitContext(t *testing.T) {
	flow := &Flow{
		Trigger: Trigger{Type: "message"},
		Nodes: []Node{
			{
				ID:          "agent",
				Kind:        "agent",
				Instruction: "Atiende a {contact_name}; entrada {input_type}",
				Outputs:     []string{"done"},
			},
			{ID: "end", Kind: "action", Action: "end"},
		},
		Edges: []Edge{
			{Source: "trigger", Target: "agent"},
			{Source: "agent", SourceHandle: "done", Target: "end"},
		},
	}

	deps := Deps{
		InputType: "text",
		Context: map[string]string{
			"contact_name":         "Ana",
			"data_facturas_numero": "F-001",
		},
		Agent: func(request AgentRequest) (string, string, error) {
			instruction, vars := request.Instruction, request.Vars
			if instruction != "Atiende a Ana; entrada text" {
				t.Fatalf("variables explícitas no interpoladas: %q", instruction)
			}
			if len(vars) != 2 || vars["input"] != "hola" || vars["input_type"] != "text" {
				t.Fatalf("el agente recibió contexto implícito: %+v", vars)
			}
			return "Listo", "done", nil
		},
	}

	result, err := Advance(flow, nil, "hola", deps)
	if err != nil || !result.Done {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestToolJSONResultCreatesTypedVariables(t *testing.T) {
	flow := &Flow{
		Trigger: Trigger{Type: "message", Match: "any"},
		Nodes: []Node{
			{ID: "tool", Kind: "tool", ToolRef: "data_mutate", SaveAs: "receipt", Args: map[string]string{
				"object": "cobros", "operation": "create", "idempotencyKey": "payment:{input.id}",
				"field.estado": "valid", "field.monto": "45.5",
			}},
			{ID: "valid", Kind: "condition", Expression: "receipt.data.estado == valid"},
			{ID: "ok", Kind: "send", Body: "importe {receipt.data.monto}"},
			{ID: "bad", Kind: "send", Body: "revisar"},
		},
		Edges: []Edge{
			{Source: "trigger", Target: "tool"},
			{Source: "tool", SourceHandle: "ok", Target: "valid"},
			{Source: "valid", SourceHandle: "true", Target: "ok"},
			{Source: "valid", SourceHandle: "false", Target: "bad"},
		},
	}
	result, err := Advance(flow, nil, "", Deps{InputType: "image", Tool: func(string, map[string]string, map[string]string) (string, error) {
		return `{"data":{"estado":"valid","monto":45.5}}`, nil
	}})
	if err != nil || len(result.Sends) != 1 || result.Sends[0] != "importe 45.5" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestAgentErrorPersistsDirectRetryState(t *testing.T) {
	flow := &Flow{
		Trigger: Trigger{Type: "message"},
		Nodes: []Node{
			{
				ID:          "agent",
				Kind:        "agent",
				Instruction: "Orienta",
				Outputs:     []string{"done"},
				ContextMode: "recent",
			},
			{ID: "end", Kind: "action", Action: "end"},
		},
		Edges: []Edge{
			{Source: "trigger", Target: "agent"},
			{Source: "agent", SourceHandle: "done", Target: "end"},
		},
	}

	calls := 0
	deps := Deps{InputType: "text"}
	deps.Agent = func(request AgentRequest) (string, string, error) {
		history := request.History
		calls++
		if calls == 1 {
			return "", "", errors.New("proveedor temporalmente no disponible")
		}
		if len(history) != 2 ||
			history[0].Content != "primer intento" ||
			history[1].Content != "segundo intento" {
			t.Fatalf("historial de reintento inesperado: %+v", history)
		}
		return "Continuemos.", "done", nil
	}

	failed, err := Advance(flow, nil, "primer intento", deps)
	if err == nil || failed.State == nil || !failed.State.ResumeDirect ||
		failed.State.NodeID != "agent" {
		t.Fatalf("estado de error no reintentable: result=%+v err=%v", failed, err)
	}

	retried, err := Advance(flow, failed.State, "segundo intento", deps)
	if err != nil || !retried.Done || calls != 2 {
		t.Fatalf("reintento: result=%+v calls=%d err=%v", retried, calls, err)
	}
}

// El valor del bloque `data_query` está en lo que deja para el Router siguiente.
// Sin esta proyección el grafo tendría el JSON entero en una variable y ninguna
// forma de decidir con él sin expresiones sobre texto.
func TestDataQueryProyectaVariablesParaElRouter(t *testing.T) {
	flow := &Flow{
		ID: "f", Name: "F", Trigger: Trigger{Type: "message", Match: "any"},
		Nodes: []Node{
			{ID: "read", Kind: "tool", ToolRef: "data_query", SaveAs: "perfil", Args: map[string]string{
				"object": "perfiles_contacto", "linkCurrentContact": "true",
				"where.1.field": "activo", "where.1.op": "eq", "where.1.value": "true",
			}},
			{ID: "router", Kind: "router", Cases: []RouterCase{
				{ID: "segmentado", Expression: `perfil.found == true && perfil.first.data.segmento_key != ""`},
			}},
			{ID: "con_perfil", Kind: "send", Body: "Segmento: {perfil.first.data.segmento_key}"},
			{ID: "sin_perfil", Kind: "send", Body: "Sin perfil"},
			{ID: "error", Kind: "send", Body: "Falló la lectura"},
		},
		Edges: []Edge{
			{ID: "e0", Source: "trigger", Target: "read"},
			{ID: "e1", Source: "read", SourceHandle: "ok", Target: "router"},
			{ID: "e2", Source: "read", SourceHandle: "error", Target: "error"},
			{ID: "e3", Source: "router", SourceHandle: "segmentado", Target: "con_perfil"},
			{ID: "e4", Source: "router", SourceHandle: "default", Target: "sin_perfil"},
		},
	}

	var gotArgs map[string]string
	deps := func(result string, err error) Deps {
		return Deps{Tool: func(_ string, args, _ map[string]string) (string, error) {
			gotArgs = args
			return result, err
		}}
	}

	encontrado, e := Advance(flow, nil, "hola", deps(
		`{"found":true,"count":1,"first":{"recordId":"r1","data":{"segmento_key":"ventas_b2b","activo":true}},"records":[]}`, nil))
	if e != nil {
		t.Fatal(e)
	}
	if len(encontrado.Sends) != 1 || encontrado.Sends[0] != "Segmento: ventas_b2b" {
		t.Fatalf("sends=%v", encontrado.Sends)
	}
	if gotArgs["where.1.value"] != "true" || gotArgs["object"] != "perfiles_contacto" {
		t.Fatalf("los argumentos no llegaron al ejecutor: %v", gotArgs)
	}

	// Cero coincidencias sale por `ok`, no por `error`: el Router debe poder
	// distinguir «no tiene perfil» de «la consulta falló».
	vacio, e := Advance(flow, nil, "hola", deps(`{"found":false,"count":0,"first":null,"records":[]}`, nil))
	if e != nil {
		t.Fatal(e)
	}
	if len(vacio.Sends) != 1 || vacio.Sends[0] != "Sin perfil" {
		t.Fatalf("sends=%v", vacio.Sends)
	}

	fallo, e := Advance(flow, nil, "hola", deps("", errors.New("objeto inexistente")))
	if e != nil {
		t.Fatal(e)
	}
	if len(fallo.Sends) != 1 || fallo.Sends[0] != "Falló la lectura" {
		t.Fatalf("sends=%v", fallo.Sends)
	}
}

// La rama de error de una herramienta puede contarle al cliente **por qué**
// falló. El motor descartaba ese texto: la herramienta conservaba el mensaje de
// dominio de la tienda —«stock insuficiente para X (disponible: 2)»— y el flujo
// solo podía responder un genérico, porque no existía ninguna variable con él.
func TestToolErrorQuedaDisponibleEnLaRamaDeError(t *testing.T) {
	flow := &Flow{ID: "f", Name: "F", Trigger: Trigger{Type: "message", Match: "any"}, Nodes: []Node{
		{ID: "pedido", Kind: "tool", ToolRef: "data_mutate", SaveAs: "pedido"},
		{ID: "ok", Kind: "send", Body: "listo"},
		{ID: "falla", Kind: "send", Body: "No pude registrarlo: {pedido.error}"},
	}, Edges: []Edge{
		{ID: "start", Source: "trigger", Target: "pedido"},
		{ID: "ok", Source: "pedido", SourceHandle: "ok", Target: "ok"},
		{ID: "error", Source: "pedido", SourceHandle: "error", Target: "falla"},
	}}

	result, err := Advance(flow, nil, "compro", Deps{
		InputType: "text",
		Tool: func(string, map[string]string, map[string]string) (string, error) {
			return "", errors.New("stock insuficiente para 'ESP32' (disponible: 2, solicitado: 5)")
		},
	})
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(result.Sends) != 1 {
		t.Fatalf("mensajes: %v", result.Sends)
	}
	want := "No pude registrarlo: stock insuficiente para 'ESP32' (disponible: 2, solicitado: 5)"
	if result.Sends[0] != want {
		t.Fatalf("el motor perdió la causa:\n  got  %q\n  want %q", result.Sends[0], want)
	}

	// Un acierto no deja la variable puesta: si lo hiciera, un {pedido.error} mal
	// colocado imprimiría el error de una ejecución anterior.
	ok, err := Advance(flow, nil, "compro", Deps{
		InputType: "text",
		Tool: func(string, map[string]string, map[string]string) (string, error) {
			return `{"orderId":11}`, nil
		},
	})
	if err != nil || len(ok.Sends) != 1 || ok.Sends[0] != "listo" {
		t.Fatalf("camino correcto: %v %v", ok.Sends, err)
	}
}
