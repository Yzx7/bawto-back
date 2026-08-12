package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type mockProvider struct {
	responses []ModelResponse
	index     int
}

func (m *mockProvider) Next(ctx context.Context, req ModelRequest) (ModelResponse, error) {
	if m.index >= len(m.responses) {
		return ModelResponse{}, errors.New("no more mock responses")
	}
	resp := m.responses[m.index]
	m.index++
	return resp, nil
}

func validMinimalFlow() json.RawMessage {
	return json.RawMessage(`{
		"trigger": {"type": "message"},
		"nodes": [
			{"id": "entry", "kind": "entry", "outputs": ["text"]}
		],
		"edges": []
	}`)
}

func TestRunTurnGracefulDegradationOnLastStepWithText(t *testing.T) {
	mock := &mockProvider{
		responses: []ModelResponse{
			{
				Calls: []FunctionCall{
					{
						ID:        "call_1",
						Name:      ToolGetFlowOutline,
						Arguments: json.RawMessage(`{}`),
					},
				},
				Usage: ModelUsage{InputTokens: 100, OutputTokens: 20},
			},
			{
				// En el paso 2 (último paso de este config), el modelo no devuelve llamadas a herramientas sino texto explicativo
				Calls: []FunctionCall{},
				Text:  "No se pudo completar la propuesta porque falta definir el segmento del cliente.",
				Usage: ModelUsage{InputTokens: 200, OutputTokens: 40},
			},
		},
	}

	config := RunnerConfig{
		MaxSteps:               2,
		MaxOperations:          10,
		MaxToolResultBytes:     1024,
		MaxIdenticalCalls:      2,
		InvalidTerminalRetries: 1,
		Timeout:                5 * time.Second,
	}

	request := TurnRequest{
		CurrentFlow: validMinimalFlow(),
		UserRequest: "Añade un router para compras",
	}

	result, err := runTurn(context.Background(), mock, config, request)
	if err != nil {
		t.Fatalf("se esperaba degradado a question sin error, pero dio: %v", err)
	}

	if result == nil {
		t.Fatal("resultado no debe ser nil")
	}

	if result.Terminal.Mode != TerminalQuestion {
		t.Errorf("se esperaba modo %s, dio %s", TerminalQuestion, result.Terminal.Mode)
	}

	if result.Terminal.Response != "No se pudo completar la propuesta porque falta definir el segmento del cliente." {
		t.Errorf("respuesta inesperada: %q", result.Terminal.Response)
	}
}

func TestRunTurnGracefulDegradationOnTerminalSubmitError(t *testing.T) {
	mock := &mockProvider{
		responses: []ModelResponse{
			{
				// Paso 1: submit_proposal pero con expectedCandidateChecksum inválido
				Calls: []FunctionCall{
					{
						ID:   "call_submit",
						Name: ToolSubmitProposal,
						Arguments: json.RawMessage(`{
							"mode": "proposal",
							"response": "He diseñado el flujo con las ramas solicitadas.",
							"expectedCandidateChecksum": "sha256:invalido"
						}`),
					},
				},
				Usage: ModelUsage{InputTokens: 150, OutputTokens: 50},
			},
		},
	}

	config := RunnerConfig{
		MaxSteps:               1, // 1 solo paso para forzar límite
		MaxOperations:          10,
		MaxToolResultBytes:     1024,
		MaxIdenticalCalls:      2,
		InvalidTerminalRetries: 0,
		Timeout:                5 * time.Second,
	}

	request := TurnRequest{
		CurrentFlow: validMinimalFlow(),
		UserRequest: "Añade un agente",
	}

	result, err := runTurn(context.Background(), mock, config, request)
	if err != nil {
		t.Fatalf("se esperaba degradado a question en submit inválido en último paso, dio: %v", err)
	}

	if result == nil {
		t.Fatal("resultado no debe ser nil")
	}

	if result.Terminal.Mode != TerminalQuestion {
		t.Errorf("se esperaba modo %s, dio %s", TerminalQuestion, result.Terminal.Mode)
	}

	if result.Terminal.Response != "He diseñado el flujo con las ramas solicitadas." {
		t.Errorf("respuesta inesperada: %q", result.Terminal.Response)
	}

	if len(result.Terminal.Warnings) == 0 {
		t.Error("se esperaba al menos un warning explicando que la propuesta no pudo aplicarse")
	}
}

func TestRunTurnGracefulDegradationOnLastStepWithoutText(t *testing.T) {
	mock := &mockProvider{
		responses: []ModelResponse{
			{
				// Paso 1: llamada a get_nodes pero sin texto ni submit_proposal (se agotan los pasos)
				Calls: []FunctionCall{
					{
						ID:        "call_1",
						Name:      ToolGetFlowOutline,
						Arguments: json.RawMessage(`{}`),
					},
				},
				Usage: ModelUsage{InputTokens: 100, OutputTokens: 20},
			},
		},
	}

	config := RunnerConfig{
		MaxSteps:               1,
		MaxOperations:          10,
		MaxToolResultBytes:     1024,
		MaxIdenticalCalls:      2,
		InvalidTerminalRetries: 0,
		Timeout:                5 * time.Second,
	}

	request := TurnRequest{
		CurrentFlow: validMinimalFlow(),
		UserRequest: "Crea un flujo",
	}

	result, err := runTurn(context.Background(), mock, config, request)
	if err != nil {
		t.Fatalf("se esperaba degradado a question sin error, pero dio: %v", err)
	}

	if result == nil {
		t.Fatal("resultado no debe ser nil")
	}

	if result.Terminal.Mode != TerminalQuestion {
		t.Errorf("se esperaba modo %s, dio %s", TerminalQuestion, result.Terminal.Mode)
	}

	if result.Terminal.Response == "" {
		t.Error("se esperaba un mensaje preguntando confirmación para continuar")
	}
}

func TestRunTurnGracefulDegradationOnRepeatedToolCalls(t *testing.T) {
	mock := &mockProvider{
		responses: []ModelResponse{
			{
				// Paso 1: llamada 1 a get_nodes
				Calls: []FunctionCall{{ID: "call_1", Name: ToolGetNodes, Arguments: json.RawMessage(`{"ids":["entry"]}`)}},
				Usage: ModelUsage{InputTokens: 100, OutputTokens: 20},
			},
			{
				// Paso 2: llamada 2 (idéntica)
				Calls: []FunctionCall{{ID: "call_2", Name: ToolGetNodes, Arguments: json.RawMessage(`{"ids":["entry"]}`)}},
				Usage: ModelUsage{InputTokens: 120, OutputTokens: 20},
			},
			{
				// Paso 3: llamada 3 (idéntica > MaxIdenticalCalls=2 -> advertencia feedback)
				Calls: []FunctionCall{{ID: "call_3", Name: ToolGetNodes, Arguments: json.RawMessage(`{"ids":["entry"]}`)}},
				Usage: ModelUsage{InputTokens: 140, OutputTokens: 20},
			},
			{
				// Paso 4: llamada 4 (persiste en la misma llamada idéntica -> degradado suave)
				Calls: []FunctionCall{{ID: "call_4", Name: ToolGetNodes, Arguments: json.RawMessage(`{"ids":["entry"]}`)}},
				Text:  "No pude encontrar más información sobre el nodo.",
				Usage: ModelUsage{InputTokens: 160, OutputTokens: 20},
			},
		},
	}

	config := RunnerConfig{
		MaxSteps:               10,
		MaxOperations:          10,
		MaxToolResultBytes:     1024,
		MaxIdenticalCalls:      2,
		InvalidTerminalRetries: 1,
		Timeout:                5 * time.Second,
	}

	request := TurnRequest{
		CurrentFlow: validMinimalFlow(),
		UserRequest: "Inspecciona el flujo",
	}

	result, err := runTurn(context.Background(), mock, config, request)
	if err != nil {
		t.Fatalf("se esperaba degradado a explanation en llamadas repetidas, pero dio: %v", err)
	}

	if result == nil {
		t.Fatal("resultado no debe ser nil")
	}

	if result.Terminal.Mode != TerminalExplanation {
		t.Errorf("se esperaba modo %s, dio %s", TerminalExplanation, result.Terminal.Mode)
	}

	if result.Terminal.Response != "No pude encontrar más información sobre el nodo." {
		t.Errorf("respuesta inesperada: %q", result.Terminal.Response)
	}
}

type timeoutProvider struct{}

func (t *timeoutProvider) Next(ctx context.Context, req ModelRequest) (ModelResponse, error) {
	return ModelResponse{}, context.DeadlineExceeded
}

func TestRunTurnGracefulDegradationOnTimeout(t *testing.T) {
	config := RunnerConfig{
		MaxSteps:               5,
		MaxOperations:          10,
		MaxToolResultBytes:     1024,
		MaxIdenticalCalls:      2,
		InvalidTerminalRetries: 0,
		Timeout:                5 * time.Second,
	}

	request := TurnRequest{
		CurrentFlow: validMinimalFlow(),
		UserRequest: "Construye el flujo",
	}

	result, err := runTurn(context.Background(), &timeoutProvider{}, config, request)
	if err != nil {
		t.Fatalf("se esperaba degradado suave a question ante timeout, pero dio error: %v", err)
	}

	if result == nil {
		t.Fatal("resultado no debe ser nil")
	}

	if result.Terminal.Mode != TerminalQuestion {
		t.Errorf("se esperaba modo %s, dio %s", TerminalQuestion, result.Terminal.Mode)
	}

	if result.Terminal.Response == "" {
		t.Error("se esperaba mensaje preguntando confirmación para continuar tras el timeout")
	}
}

func TestRunTurnHandlesUnknownToolGracefully(t *testing.T) {
	mock := &mockProvider{
		responses: []ModelResponse{
			{
				// Paso 1: el modelo llama a una función no declarada (e.g. report_authoring_issue)
				Calls: []FunctionCall{
					{
						ID:        "call_unknown",
						Name:      "report_authoring_issue",
						Arguments: json.RawMessage(`{"issue":"no hay catálogo"}`),
					},
				},
				Usage: ModelUsage{InputTokens: 100, OutputTokens: 30},
			},
			{
				// Paso 2: al recibir el feedback del error, el modelo llama correctamente a submit_proposal (mode=question)
				Calls: []FunctionCall{
					{
						ID:   "call_submit",
						Name: ToolSubmitProposal,
						Arguments: json.RawMessage(`{
							"mode": "question",
							"response": "Falta definir si se vende el libro A o B."
						}`),
					},
				},
				Usage: ModelUsage{InputTokens: 150, OutputTokens: 40},
			},
		},
	}

	config := RunnerConfig{
		MaxSteps:               5,
		MaxOperations:          10,
		MaxToolResultBytes:     1024,
		MaxIdenticalCalls:      2,
		InvalidTerminalRetries: 1,
		Timeout:                5 * time.Second,
	}

	request := TurnRequest{
		CurrentFlow: validMinimalFlow(),
		UserRequest: "Ayúdame con el flujo",
		RecentConversation: []ConversationEntry{
			{Role: "user", Content: "Quiero vender un libro"},
			{Role: "assistant", Content: "¿De qué temática?"},
		},
	}

	result, err := runTurn(context.Background(), mock, config, request)
	if err != nil {
		t.Fatalf("se esperaba que la herramienta desconocida devolviera error a la IA y permitiera continuar, pero falló con: %v", err)
	}

	if result == nil {
		t.Fatal("resultado no debe ser nil")
	}

	if result.Terminal.Mode != TerminalQuestion {
		t.Errorf("se esperaba modo %s, dio %s", TerminalQuestion, result.Terminal.Mode)
	}

	if result.Terminal.Response != "Falta definir si se vende el libro A o B." {
		t.Errorf("respuesta inesperada: %q", result.Terminal.Response)
	}
}
