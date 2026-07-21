package engine

import (
	"encoding/json"
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
	agent := func(_ string, vars map[string]string, outputs []string) (string, string, error) {
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
		Agent: func(instr string, vars map[string]string, outs []string) (string, string, error) {
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
}
