package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/Yzx7/sacs-chatbots/engine"
)

func TestAgentRunUsesForcedBranchTool(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"msg_1",
			"type":"message",
			"role":"assistant",
			"model":"MiniMax-M2",
			"content":[{
				"type":"tool_use",
				"id":"tool_1",
				"name":"select_flow_branch",
				"input":{"branch":"valido","reply":"Gracias, tu pago fue confirmado."}
			}],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":10,"output_tokens":8,"cache_read_input_tokens":4,"cache_creation_input_tokens":2}
		}`))
	}))
	defer srv.Close()

	a := New("sk-test", srv.URL, "MiniMax-M2")
	reply, branch, err := a.Run(context.Background(), "Valida el comprobante",
		map[string]string{"input": "aquí está mi pago"}, []string{"valido", "invalido"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if branch != "valido" || reply != "Gracias, tu pago fue confirmado." {
		t.Fatalf("reply=%q branch=%q", reply, branch)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("path esperado /v1/messages, got %q", gotPath)
	}
	if gotBody["model"] != "MiniMax-M2" {
		t.Fatalf("model esperado MiniMax-M2, got %v", gotBody["model"])
	}

	toolChoice, ok := gotBody["tool_choice"].(map[string]any)
	if !ok || toolChoice["type"] != "tool" || toolChoice["name"] != branchToolName {
		t.Fatalf("tool_choice inesperado: %#v", gotBody["tool_choice"])
	}
	if toolChoice["disable_parallel_tool_use"] != true {
		t.Fatalf("tool_choice permite llamadas paralelas: %#v", toolChoice)
	}
	if gotBody["temperature"] != 0.2 {
		t.Fatalf("temperature inesperada: %#v", gotBody["temperature"])
	}
	tools, ok := gotBody["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools inesperado: %#v", gotBody["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	schema, _ := tool["input_schema"].(map[string]any)
	if schema["additionalProperties"] != false {
		t.Fatalf("schema permite propiedades adicionales: %#v", schema)
	}
	properties, _ := schema["properties"].(map[string]any)
	branchProperty, _ := properties["branch"].(map[string]any)
	enum, _ := branchProperty["enum"].([]any)
	if len(enum) != 2 || enum[0] != "valido" || enum[1] != "invalido" {
		t.Fatalf("enum inesperado: %#v", enum)
	}
}

func TestAgentRunWithUsageTextOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"req_cost_1","type":"message","role":"assistant","model":"MiniMax-M2","content":[{"type":"text","text":"Listo"}],"stop_reason":"end_turn","usage":{"input_tokens":1000,"output_tokens":500,"cache_read_input_tokens":200,"cache_creation_input_tokens":100}}`))
	}))
	defer srv.Close()

	rates := Rates{InputPerMillion: 0.30, OutputPerMillion: 1.20, CacheReadPerMillion: 0.03, CacheWritePerMillion: 0.375}
	a := NewWithPricing("sk-test", srv.URL, "minimax", "MiniMax-M2", rates)
	reply, branch, usage, err := a.RunWithUsage(context.Background(), "Responde", map[string]string{"input": "hola"}, nil)
	if err != nil {
		t.Fatalf("RunWithUsage: %v", err)
	}
	if reply != "Listo" || branch != "" {
		t.Fatalf("reply=%q branch=%q", reply, branch)
	}
	if usage.RequestID != "req_cost_1" || usage.InputTokens != 1000 || usage.OutputTokens != 500 ||
		usage.CacheReadInputTokens != 200 || usage.CacheCreationInputTokens != 100 {
		t.Fatalf("usage inesperado: %+v", usage)
	}
	const want = 0.0009435
	if got := usage.EstimatedCostUSD(); got < want-1e-12 || got > want+1e-12 {
		t.Fatalf("costo esperado %.7f, got %.10f", want, got)
	}
}

func TestInvalidStructuredOutputKeepsUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"req_invalid","type":"message","role":"assistant","model":"MiniMax-M2","content":[{"type":"text","text":"No invoqué la función"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":7}}`))
	}))
	defer srv.Close()

	a := New("sk-test", srv.URL, "MiniMax-M2")
	_, _, usage, err := a.RunWithUsage(context.Background(), "Decide", nil, []string{"a", "b"})
	if err == nil || OutputErrorCode(err) != "missing_tool_call" {
		t.Fatalf("error inesperado: %v", err)
	}
	if usage.RequestID != "req_invalid" || usage.InputTokens != 12 || usage.OutputTokens != 7 {
		t.Fatalf("el error no debe perder usage: %+v", usage)
	}
}

func TestRunWithHistoryUsageSendsOnlyConversation(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"req_history",
			"type":"message",
			"role":"assistant",
			"model":"MiniMax-M2",
			"content":[{
				"type":"tool_use",
				"id":"tool_history",
				"name":"select_flow_branch",
				"input":{"branch":"ecommerce","reply":"Una tienda parece la mejor opción."}
			}],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":40,"output_tokens":12}
		}`))
	}))
	defer srv.Close()

	a := New("sk-test", srv.URL, "MiniMax-M2")
	history := []engine.ChatMessage{
		{Role: "user", Content: "Tengo un negocio"},
		{Role: "assistant", Content: "¿Qué deseas mejorar?"},
		{Role: "user", Content: "Quiero vender por internet"},
	}
	reply, branch, _, err := a.RunWithHistoryUsage(
		context.Background(), "Recomienda un servicio",
		map[string]string{
			"input":                "Quiero vender por internet",
			"contact_name":         "Ana",
			"data_facturas_numero": "F-001",
		},
		[]string{"ecommerce", "otro"}, history, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if branch != "ecommerce" || !strings.Contains(reply, "tienda") {
		t.Fatalf("respuesta=%q branch=%q", reply, branch)
	}

	messages, ok := gotBody["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages inesperados: %#v", gotBody["messages"])
	}
	message, ok := messages[0].(map[string]any)
	if !ok || message["role"] != "user" {
		t.Fatalf("mensaje inesperado: %#v", messages[0])
	}
	content, _ := message["content"].([]any)
	textBlock, _ := content[0].(map[string]any)
	transcript, _ := textBlock["text"].(string)
	for _, want := range []string{
		"Cliente: Tengo un negocio",
		"Asistente: ¿Qué deseas mejorar?",
		"Cliente: Quiero vender por internet",
	} {
		if !strings.Contains(transcript, want) {
			t.Fatalf("transcripción sin %q: %s", want, transcript)
		}
	}
	requestJSON, _ := json.Marshal(gotBody)
	if strings.Contains(string(requestJSON), "contact_name") ||
		strings.Contains(string(requestJSON), "F-001") {
		t.Fatalf("se filtraron datos no solicitados al prompt: %s", requestJSON)
	}
}

func TestSilentAgentOnlyRequiresBranch(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"req_silent",
			"type":"message",
			"role":"assistant",
			"model":"MiniMax-M2",
			"content":[{"type":"tool_use","id":"tool_silent","name":"select_flow_branch","input":{"branch":"menu"}}],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":10,"output_tokens":4}
		}`))
	}))
	defer srv.Close()

	a := New("sk-test", srv.URL, "MiniMax-M2")
	reply, branch, _, err := a.RunWithHistoryUsage(
		context.Background(), "Clasifica", map[string]string{"input": "hola"},
		[]string{"menu", "asesor"}, nil, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reply != "" || branch != "menu" {
		t.Fatalf("reply=%q branch=%q", reply, branch)
	}

	tools, _ := gotBody["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	schema, _ := tool["input_schema"].(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	if _, exists := properties["reply"]; exists {
		t.Fatalf("un agente silencioso no debe solicitar reply: %#v", schema)
	}
}

func TestParseBranchToolRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name string
		json string
		code string
	}{
		{
			name: "missing",
			json: `[{"type":"text","text":"solo texto"}]`,
			code: "missing_tool_call",
		},
		{
			name: "unexpected tool",
			json: `[{"type":"tool_use","id":"x","name":"otra","input":{"branch":"a","reply":"ok"}}]`,
			code: "unexpected_tool",
		},
		{
			name: "multiple",
			json: `[{"type":"tool_use","id":"x","name":"select_flow_branch","input":{"branch":"a","reply":"ok"}},{"type":"tool_use","id":"y","name":"select_flow_branch","input":{"branch":"a","reply":"ok"}}]`,
			code: "multiple_tool_calls",
		},
		{
			name: "unknown branch",
			json: `[{"type":"tool_use","id":"x","name":"select_flow_branch","input":{"branch":"c","reply":"ok"}}]`,
			code: "invalid_branch",
		},
		{
			name: "extra property",
			json: `[{"type":"tool_use","id":"x","name":"select_flow_branch","input":{"branch":"a","reply":"ok","other":true}}]`,
			code: "invalid_tool_input",
		},
		{
			name: "empty reply",
			json: `[{"type":"tool_use","id":"x","name":"select_flow_branch","input":{"branch":"a","reply":"  "}}]`,
			code: "empty_reply",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var content []anthropic.ContentBlockUnion
			if err := json.Unmarshal([]byte(tt.json), &content); err != nil {
				t.Fatal(err)
			}
			_, _, err := parseBranchTool(content, []string{"a", "b"}, false)
			if err == nil || OutputErrorCode(err) != tt.code {
				t.Fatalf("error=%v code=%q", err, OutputErrorCode(err))
			}
		})
	}
}

func TestBranchToolRejectsInvalidOutputs(t *testing.T) {
	tests := [][]string{
		{""},
		{" a"},
		{"a", "A"},
	}
	for _, outputs := range tests {
		if _, err := branchTool(outputs, false); err == nil ||
			OutputErrorCode(err) != "invalid_outputs" {
			t.Fatalf("outputs=%q err=%v", outputs, err)
		}
	}
}

// DeepSeek trae el razonamiento encendido en esfuerzo `high` si no se dice lo
// contrario, y eso se paga como tokens de salida y como latencia. El campo va
// por WithJSONSet porque `reasoning` no existe en la API de Anthropic, así que
// nada de tipos lo protege: si alguien lo rompe, solo lo dice esta prueba.
func TestAgenteDeepSeekApagaElRazonamiento(t *testing.T) {
	cuerpo := capturarPeticion(t, "deepseek", "deepseek-v4-flash")

	thinking, ok := cuerpo["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("falta el campo thinking en la peticion: %#v", cuerpo)
	}
	if thinking["type"] != "disabled" {
		t.Fatalf("se esperaba thinking.type=disabled, llego %#v", thinking["type"])
	}
	// `reasoning` es lo que documenta DeepSeek para formato Anthropic y no
	// funciona: comprobado contra la API real. Si alguien lo reintroduce
	// creyendo el doc, el razonamiento vuelve a quedarse encendido.
	if _, existe := cuerpo["reasoning"]; existe {
		t.Fatal("reasoning no apaga nada en este endpoint; el campo bueno es thinking")
	}
}

// El campo es una extensión de DeepSeek: mandárselo a otro proveedor es pedirle
// que ignore algo que no entiende, y no todos ignoran en silencio.
func TestAgenteMiniMaxNoMandaRazonamiento(t *testing.T) {
	cuerpo := capturarPeticion(t, "minimax", "MiniMax-M3")

	if _, existe := cuerpo["thinking"]; existe {
		t.Fatalf("no deberia mandarse thinking a minimax: %#v", cuerpo["thinking"])
	}
}

func capturarPeticion(t *testing.T, provider, model string) map[string]any {
	t.Helper()
	var cuerpo map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &cuerpo)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m",
			"content":[{"type":"tool_use","id":"t1","name":"select_flow_branch",
			"input":{"branch":"a","reply":"hola"}}],
			"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	a := NewWithPricing("sk-test", srv.URL, provider, model, Rates{})
	if _, _, err := a.Run(context.Background(), "instrucción",
		map[string]string{"input": "hola"}, []string{"a", "b"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return cuerpo
}

// El caché del proveedor es por prefijo de contenido, no por sesión: sin esta
// etiqueta todas las conversaciones de la cuenta comparten bolsa. Comprobado
// contra la API real: el mismo prompt con otra etiqueta entra en frio.
func TestAgenteAislaElCachePorInquilino(t *testing.T) {
	cuerpo := capturarPeticionInquilino(t, "minimax", "MiniMax-M3", "org-123")

	metadata, ok := cuerpo["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("falta metadata en la peticion: %#v", cuerpo)
	}
	if metadata["user_id"] != "org-123" {
		t.Fatalf("se esperaba user_id=org-123, llego %#v", metadata["user_id"])
	}
}

// Sin inquilino no se manda el campo en vez de mandarlo vacio: una etiqueta ""
// seria un inquilino mas, y todo lo que no la ponga caeria en su bolsa.
func TestAgenteSinInquilinoNoMandaMetadata(t *testing.T) {
	cuerpo := capturarPeticionInquilino(t, "minimax", "MiniMax-M3", "   ")

	if _, existe := cuerpo["metadata"]; existe {
		t.Fatalf("no deberia mandarse metadata sin inquilino: %#v", cuerpo["metadata"])
	}
}

// ForTenant devuelve una copia: si mutara el agente compartido, la etiqueta de
// una organizacion se quedaria pegada para la siguiente peticion de otra.
func TestForTenantNoMutaElAgenteCompartido(t *testing.T) {
	base := NewWithPricing("sk-test", "http://localhost", "minimax", "m", Rates{})
	derivado := base.ForTenant("org-A")

	if base.tenant != "" {
		t.Fatalf("ForTenant mutó el agente compartido: %q", base.tenant)
	}
	if derivado.tenant != "org-A" {
		t.Fatalf("la copia no llevó el inquilino: %q", derivado.tenant)
	}
	if otra := base.ForTenant("org-B"); otra.tenant != "org-B" || derivado.tenant != "org-A" {
		t.Fatalf("las copias se pisan: %q / %q", otra.tenant, derivado.tenant)
	}
}

func capturarPeticionInquilino(t *testing.T, provider, model, tenant string) map[string]any {
	t.Helper()
	var cuerpo map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &cuerpo)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m",
			"content":[{"type":"text","text":"hola"}],
			"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	a := NewWithPricing("sk-test", srv.URL, provider, model, Rates{}).ForTenant(tenant)
	if _, _, err := a.Run(context.Background(), "instrucción",
		map[string]string{"input": "hola"}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return cuerpo
}
