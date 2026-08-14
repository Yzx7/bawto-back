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

func TestWAACreditosFlowSeparaEscaneoVisualDeValidacion(t *testing.T) {
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
	retryScan := nodes["n_classify_payment_retry"]
	completeRouter := nodes["n_receipt_complete_router"]
	retryCompleteRouter := nodes["n_receipt_retry_complete_router"]
	copyRetry := nodes["n_copy_receipt_retry"]
	savePayment := nodes["n_save_payment"]
	activateTool := nodes["n_activate_subscription"]
	paymentHasCode := nodes["n_payment_needs_review"]
	askOrgCode := nodes["n_ask_org_code"]
	existingReceiptRouter := nodes["n_existing_receipt_router"]
	paymentSpecialist := nodes["n_payment_specialist"]

	if specialist == nil || classifyPayment == nil || retryScan == nil || completeRouter == nil ||
		retryCompleteRouter == nil || copyRetry == nil || savePayment == nil || activateTool == nil ||
		paymentHasCode == nil || askOrgCode == nil || existingReceiptRouter == nil || paymentSpecialist == nil {
		t.Fatalf("faltan nodos clave en flow_waa_creditos")
	}
	for _, removed := range []string{
		"n_read_authorized_payment_methods", "n_payment_not_receipt", "n_payment_review",
		"n_save_payment_review", "n_receipt_reviewable_router",
	} {
		if nodes[removed] != nil {
			t.Errorf("el nodo visual obsoleto %s no debe permanecer", removed)
		}
	}

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

	if classifyPayment.Instruction != retryScan.Instruction {
		t.Errorf("ambos intentos deben usar exactamente la misma instrucción de escaneo")
	}
	lowerInstruction := strings.ToLower(classifyPayment.Instruction)
	for _, forbidden := range []string{
		"authorized_payment_methods", "cuentas autorizadas", "destinatario coincide",
		"pago válido", "valid", "needs_review", "confidence",
	} {
		if strings.Contains(lowerInstruction, forbidden) {
			t.Errorf("el escáner visual no debe decidir validez (%q): %q", forbidden, classifyPayment.Instruction)
		}
	}
	for _, scanner := range []*Node{classifyPayment, retryScan} {
		if len(scanner.Accepts) != 1 || scanner.Accepts[0] != "image" {
			t.Errorf("%s debe aceptar solo imágenes: %v", scanner.ID, scanner.Accepts)
		}
		if len(scanner.Outputs) != 1 || scanner.Outputs[0] != "scanned" {
			t.Errorf("%s debe producir una sola rama scanned: %v", scanner.ID, scanner.Outputs)
		}
		scanFields := map[string]string{}
		for _, field := range scanner.OutputFields {
			scanFields[field.Key] = field.Type
		}
		for _, field := range []string{"provider", "amount", "currency", "occurredAt", "operationCode", "recipient", "statusText"} {
			if scanFields[field] != "string" {
				t.Errorf("%s debe transcribir %s como string: %+v", scanner.ID, field, scanner.OutputFields)
			}
		}
		if len(scanFields) != 7 {
			t.Errorf("%s tiene campos ajenos a la transcripción: %+v", scanner.ID, scanner.OutputFields)
		}
	}
	if classifyPayment.SaveAs != "receipt" || retryScan.SaveAs != "receipt_retry" {
		t.Errorf("los dos escaneos deben conservar resultados separados: %q / %q", classifyPayment.SaveAs, retryScan.SaveAs)
	}

	if savePayment.Args["field.resultado"] != "{receipt.statusText}" {
		t.Errorf("n_save_payment debe persistir el resultado visible: %+v", savePayment.Args)
	}
	if _, exists := savePayment.Args["field.confianza"]; exists {
		t.Errorf("n_save_payment no debe guardar una confianza decidida por la IA")
	}
	if activateTool.ToolRef != "credit_recharge_activate" {
		t.Errorf("n_activate_subscription debe usar credit_recharge_activate, tiene %q", activateTool.ToolRef)
	}
	if activateTool.Args["activationCode"] != "{sale.organizationCode}" {
		t.Errorf("n_activate_subscription.activationCode = %q", activateTool.Args["activationCode"])
	}
	if activateTool.Args["creditsAmount"] != "" || activateTool.Args["amount"] != "" || activateTool.Args["idempotencyKey"] != "" {
		t.Errorf("el flujo no debe decidir monto ni idempotencia de la recarga: %+v", activateTool.Args)
	}
	if paymentHasCode.Expression != "!empty(sale.organizationCode)" {
		t.Errorf("n_payment_needs_review.Expression = %q", paymentHasCode.Expression)
	}
	if strings.Contains(askOrgCode.Body, "recibido exitosamente") {
		t.Errorf("n_ask_org_code no debe afirmar éxito antes de acreditar")
	}
	if strings.Contains(strings.Join(specialist.ReplyOn, ","), "cobrar") || strings.Contains(strings.Join(paymentSpecialist.ReplyOn, ","), "cobrar") {
		t.Errorf("los especialistas no deben responder antes de ejecutar la rama cobrar")
	}

	edges := map[string][]string{}
	for _, edge := range flow.Edges {
		edges[edge.Source+"."+edge.SourceHandle] = append(edges[edge.Source+"."+edge.SourceHandle], edge.Target)
	}
	assertEdge := func(sourceHandle, target string) {
		t.Helper()
		targets := edges[sourceHandle]
		if len(targets) != 1 || targets[0] != target {
			t.Errorf("%s debe conectar a %s, tiene %v", sourceHandle, target, targets)
		}
	}
	assertEdge("n_classify_payment.scanned", "n_receipt_complete_router")
	assertEdge("n_receipt_complete_router.complete", "n_judge_result")
	assertEdge("n_receipt_complete_router.default", "n_classify_payment_retry")
	assertEdge("n_classify_payment_retry.scanned", "n_receipt_retry_complete_router")
	assertEdge("n_receipt_retry_complete_router.complete", "n_copy_receipt_retry")
	assertEdge("n_receipt_retry_complete_router.default", "n_payment_retry")
	assertEdge("n_copy_receipt_retry.", "n_judge_result")
	assertEdge("n_judge_result.exitoso", "n_payment_needs_review")
	assertEdge("n_judge_result.no_exitoso", "n_payment_not_successful")
	assertEdge("n_judge_result.dudoso", "n_payment_not_successful")
	assertEdge("n_payment_needs_review.true", "n_save_payment")
	assertEdge("n_payment_needs_review.false", "n_ask_org_code")
	assertEdge("n_ask_org_code.out", "n_espera")
	assertEdge("n_bawto_specialist.cobrar", "n_existing_receipt_router")
	assertEdge("n_existing_receipt_router.ready", "n_save_payment")
	assertEdge("n_existing_receipt_router.incomplete", "n_payment_retry")

	if !strings.Contains(completeRouter.Cases[0].Expression, "receipt.statusText") ||
		!strings.Contains(retryCompleteRouter.Cases[0].Expression, "receipt_retry.statusText") ||
		!strings.Contains(existingReceiptRouter.Cases[0].Expression, "receipt.statusText") {
		t.Errorf("los routers deterministas deben exigir el resultado visible")
	}
	if copyRetry.Params["receipt.amount"] != "{receipt_retry.amount}" ||
		copyRetry.Params["receipt.statusText"] != "{receipt_retry.statusText}" {
		t.Errorf("n_copy_receipt_retry no copia la segunda transcripción: %+v", copyRetry.Params)
	}
}

// Quién decide si el comprobante expresa éxito es un agente del grafo, no una
// lista de frases compilada en Go. La instrucción tiene que pedir un juicio
// semántico: en cuanto enumera los textos concretos de cada app vuelve a ser un
// allowlist, con la diferencia de que ahora está escondido en un prompt.
func TestWAACreditosJuzgaElResultadoConUnAgente(t *testing.T) {
	flow := loadWAACreditosFlow(t)
	var judge *Node
	for i := range flow.Nodes {
		if flow.Nodes[i].ID == "n_judge_result" {
			judge = &flow.Nodes[i]
		}
	}
	if judge == nil {
		t.Fatal("falta n_judge_result")
	}
	if judge.Kind != "agent" {
		t.Errorf("el juicio debe emitirlo un agente, no %q", judge.Kind)
	}
	if judge.ToolRef != "" {
		t.Errorf("el juez no necesita herramientas: %q", judge.ToolRef)
	}
	if !judge.Silent {
		t.Errorf("el juez no habla con el cliente; solo decide la rama")
	}
	want := []string{"exitoso", "no_exitoso", "dudoso"}
	if strings.Join(judge.Outputs, ",") != strings.Join(want, ",") {
		t.Errorf("ramas del juez = %v, se esperaba %v", judge.Outputs, want)
	}
	if !strings.Contains(judge.Instruction, "{receipt.statusText}") {
		t.Errorf("el juez debe recibir el texto transcrito: %q", judge.Instruction)
	}
	lower := strings.ToLower(judge.Instruction)
	for _, copy := range []string{"yapeaste", "operación exitosa", "pago exitoso", "plin"} {
		if strings.Contains(lower, copy) {
			t.Errorf("la instrucción enumera el copy %q: es un allowlist disfrazado", copy)
		}
	}
}

func TestWAACreditosReescaneaAntesDePedirOtraImagen(t *testing.T) {
	flow := loadWAACreditosFlow(t)
	agentCalls := []string{}
	result, err := Advance(flow, nil, "", Deps{
		InputType: "image",
		WaID:      "wamid-reescaneo-completo",
		AgentStructured: func(request AgentRequest) (AgentResult, error) {
			agentCalls = append(agentCalls, request.NodeID)
			switch request.NodeID {
			case "n_classify_payment":
				return AgentResult{Branch: "scanned", Data: map[string]any{
					"provider": "yape", "currency": "PEN", "occurredAt": "2026-08-13T02:23:00-05:00",
					"operationCode": "10484343", "recipient": "Gerson Rod*", "statusText": "¡Yapeaste!",
				}}, nil
			case "n_classify_payment_retry":
				return AgentResult{Branch: "scanned", Data: map[string]any{
					"provider": "yape", "amount": "125.00", "currency": "PEN",
					"occurredAt": "2026-08-13T02:23:00-05:00", "operationCode": "10484343",
					"recipient": "Gerson Rod*", "statusText": "¡Yapeaste!",
				}}, nil
			case "n_judge_result":
				return AgentResult{Branch: "exitoso"}, nil
			default:
				t.Fatalf("agente inesperado: %s", request.NodeID)
				return AgentResult{}, nil
			}
		},
		Tool: func(ref string, _ map[string]string, _ map[string]string) (string, error) {
			t.Fatalf("no debe ejecutar %s antes de recibir el código de activación", ref)
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("avance con segunda lectura completa: %v", err)
	}
	if strings.Join(agentCalls, ",") != "n_classify_payment,n_classify_payment_retry,n_judge_result" {
		t.Fatalf("secuencia de escaneo inesperada: %v", agentCalls)
	}
	if result.State == nil || result.State.NodeID != "n_espera" || len(result.Sends) != 1 {
		t.Fatalf("resultado inesperado: %+v", result)
	}
	if !strings.Contains(result.Sends[0], "PEN 125.00") || !strings.Contains(result.Sends[0], "código de activación") {
		t.Fatalf("la segunda lectura no se propagó: %q", result.Sends[0])
	}
	if strings.Contains(result.Sends[0], "{receipt.amount}") || strings.Contains(result.Sends[0], "créditos acreditados") {
		t.Fatalf("respuesta insegura: %q", result.Sends[0])
	}
}

// Una captura perfectamente legible de una operación que no se completó supera
// los routers deterministas: tiene proveedor, importe, fecha, operación y
// destinatario. Solo el juicio del agente la separa de un pago real, y sin él
// se acreditaría saldo por una transferencia que nunca ocurrió.
func TestWAACreditosNoAcreditaUnaOperacionNoCompletada(t *testing.T) {
	flow := loadWAACreditosFlow(t)
	agentCalls := []string{}
	result, err := Advance(flow, nil, "", Deps{
		InputType: "image",
		WaID:      "wamid-operacion-en-proceso",
		AgentStructured: func(request AgentRequest) (AgentResult, error) {
			agentCalls = append(agentCalls, request.NodeID)
			if request.NodeID == "n_judge_result" {
				return AgentResult{Branch: "no_exitoso"}, nil
			}
			return AgentResult{Branch: "scanned", Data: map[string]any{
				"provider": "bcp", "amount": "240.00", "currency": "PEN",
				"occurredAt": "2026-08-13T02:23:00-05:00", "operationCode": "01391991",
				"recipient": "Gerson Rod*", "statusText": "Transferencia en proceso",
			}}, nil
		},
		Tool: func(ref string, _ map[string]string, _ map[string]string) (string, error) {
			t.Fatalf("una operación no completada no debe ejecutar %s", ref)
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("avance con operación en proceso: %v", err)
	}
	if strings.Join(agentCalls, ",") != "n_classify_payment,n_judge_result" {
		t.Fatalf("el juez debe evaluar la primera lectura completa: %v", agentCalls)
	}
	if result.State == nil || result.State.NodeID != "n_espera" || len(result.Sends) != 1 {
		t.Fatalf("resultado inesperado: %+v", result)
	}
	if !strings.Contains(result.Sends[0], "no muestra una operación completada") ||
		!strings.Contains(result.Sends[0], "No se acreditó ningún saldo") {
		t.Fatalf("respuesta inesperada: %q", result.Sends[0])
	}
	if strings.Contains(result.Sends[0], "código de activación") {
		t.Fatalf("no debe pedir el código de una operación que no se completó: %q", result.Sends[0])
	}
}

func TestWAACreditosDosEscaneosIncompletosPidenOtraImagen(t *testing.T) {
	flow := loadWAACreditosFlow(t)
	agentCalls := []string{}
	result, err := Advance(flow, nil, "", Deps{
		InputType: "image",
		WaID:      "wamid-dos-escaneos-incompletos",
		AgentStructured: func(request AgentRequest) (AgentResult, error) {
			agentCalls = append(agentCalls, request.NodeID)
			return AgentResult{Branch: "scanned", Data: map[string]any{
				"provider": "yape", "currency": "PEN", "occurredAt": "2026-08-13T02:23:00-05:00",
				"operationCode": "10484343", "recipient": "Gerson Rod*", "statusText": "¡Yapeaste!",
			}}, nil
		},
		Tool: func(ref string, _ map[string]string, _ map[string]string) (string, error) {
			t.Fatalf("un escaneo incompleto no debe ejecutar %s", ref)
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("avance con dos lecturas incompletas: %v", err)
	}
	if strings.Join(agentCalls, ",") != "n_classify_payment,n_classify_payment_retry" {
		t.Fatalf("secuencia de escaneo inesperada: %v", agentCalls)
	}
	if result.State == nil || result.State.NodeID != "n_espera" || len(result.Sends) != 1 {
		t.Fatalf("resultado inesperado: %+v", result)
	}
	if !strings.Contains(result.Sends[0], "otra foto") || !strings.Contains(result.Sends[0], "No se acreditó ningún saldo") {
		t.Fatalf("respuesta de reintento inesperada: %q", result.Sends[0])
	}
}
