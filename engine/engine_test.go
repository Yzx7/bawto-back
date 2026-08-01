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
	paid, err := Advance(&flow, payment.State, "voucher", Deps{InputType: "image", MediaID: "media", WaID: "wamid", Agent: agent, Tool: func(_ string, _ map[string]string, _ map[string]string) (string, error) { return "invoice", nil }})
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
