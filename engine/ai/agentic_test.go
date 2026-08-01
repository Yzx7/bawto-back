package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// El bucle agéntico tiene una exigencia que no se ve en la respuesta final: la
// contestación del modelo hay que devolverla **completa** al historial —bloques
// de pensamiento incluidos— o la cadena de razonamiento se rompe en la segunda
// petición. Esta prueba mira lo que se le manda al proveedor, no solo lo que
// devuelve la función, porque es ahí donde está el contrato.
func TestRunAgenticEncadenaHerramientaYConservaLosBloques(t *testing.T) {
	var requests []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		requests = append(requests, parsed)

		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			// Primer turno: el modelo piensa y pide la herramienta.
			_, _ = w.Write([]byte(`{
				"id":"msg_1","type":"message","role":"assistant","model":"MiniMax-M3",
				"content":[
					{"type":"thinking","thinking":"Necesito el catálogo.","signature":"sig-1"},
					{"type":"tool_use","id":"tu_1","name":"search_data","input":{"query":"tienda online"}}
				],
				"stop_reason":"tool_use",
				"usage":{"input_tokens":100,"output_tokens":20}
			}`))
			return
		}
		// Segundo turno: ya con el resultado, cierra eligiendo rama.
		_, _ = w.Write([]byte(`{
			"id":"msg_2","type":"message","role":"assistant","model":"MiniMax-M3",
			"content":[{"type":"tool_use","id":"tu_2","name":"select_flow_branch",
				"input":{"branch":"conversar","reply":"Tenemos Meudim Ecommerce."}}],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":300,"output_tokens":30}
		}`))
	}))
	defer srv.Close()

	var gotName string
	var gotInput string
	exec := func(_ context.Context, name string, input json.RawMessage) (string, error) {
		gotName, gotInput = name, string(input)
		return "1. nombre: Meudim Ecommerce · precio: desde S/ 900", nil
	}

	a := New("sk-test", srv.URL, "MiniMax-M3")
	reply, branch, usage, err := a.RunAgenticUsage(context.Background(),
		"Orienta al cliente", map[string]string{"input": "quiero vender online"},
		[]string{"conversar", "asesor"}, nil, false,
		[]AgentTool{{
			Name:        "search_data",
			Description: "Busca en el catálogo.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"query": map[string]any{"type": "string"}},
				"required":   []string{"query"},
			},
		}}, exec)
	if err != nil {
		t.Fatalf("RunAgenticUsage: %v", err)
	}

	if reply != "Tenemos Meudim Ecommerce." || branch != "conversar" {
		t.Fatalf("salida inesperada: reply=%q branch=%q", reply, branch)
	}
	if gotName != "search_data" || gotInput != `{"query":"tienda online"}` {
		t.Fatalf("la herramienta recibió %q con %s", gotName, gotInput)
	}
	if len(requests) != 2 {
		t.Fatalf("se esperaban 2 peticiones al proveedor, hubo %d", len(requests))
	}

	// El coste del turno es la suma de los pasos, no el del último.
	if usage.Steps != 2 || usage.InputTokens != 400 || usage.OutputTokens != 50 {
		t.Fatalf("consumo mal acumulado: steps=%d in=%d out=%d",
			usage.Steps, usage.InputTokens, usage.OutputTokens)
	}

	// La segunda petición debe llevar: el mensaje original, la respuesta del
	// asistente **con su bloque de pensamiento intacto**, y el tool_result.
	messages, _ := requests[1]["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("la segunda petición debería llevar 3 mensajes, lleva %d", len(messages))
	}

	assistant, _ := messages[1].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("el segundo mensaje debería ser del asistente: %v", assistant["role"])
	}
	blocks, _ := assistant["content"].([]any)
	kinds := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if b, ok := block.(map[string]any); ok {
			kinds = append(kinds, fmt.Sprint(b["type"]))
		}
	}
	if len(kinds) != 2 || kinds[0] != "thinking" || kinds[1] != "tool_use" {
		t.Fatalf("se perdieron bloques de la respuesta del modelo: %v", kinds)
	}

	result, _ := messages[2].(map[string]any)
	resultBlocks, _ := result["content"].([]any)
	first, _ := resultBlocks[0].(map[string]any)
	if first["type"] != "tool_result" || first["tool_use_id"] != "tu_1" {
		t.Fatalf("el resultado no se ató a la llamada: %v", first)
	}
}

// Un modelo que se atasca llamando herramientas no puede convertir un mensaje de
// WhatsApp en una factura abierta: el bucle corta y lo dice.
func TestRunAgenticCortaSiNuncaEligeRama(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"msg","type":"message","role":"assistant","model":"MiniMax-M3",
			"content":[{"type":"tool_use","id":"tu","name":"search_data","input":{"query":"x"}}],
			"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}
		}`))
	}))
	defer srv.Close()

	exec := func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
		return "nada", nil
	}
	a := New("sk-test", srv.URL, "MiniMax-M3")
	_, _, usage, err := a.RunAgenticUsage(context.Background(), "instrucción",
		map[string]string{"input": "hola"}, []string{"conversar"}, nil, false,
		[]AgentTool{{Name: "search_data", Description: "Busca.", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}},
		}}}, exec)

	if OutputErrorCode(err) != "tool_loop_exhausted" {
		t.Fatalf("se esperaba tool_loop_exhausted, got %v", err)
	}
	if calls != maxAgentSteps || usage.Steps != maxAgentSteps {
		t.Fatalf("el tope no se respetó: llamadas=%d steps=%d", calls, usage.Steps)
	}
}

// Un fallo de la herramienta no aborta el turno: se le entrega al modelo como
// resultado marcado como error para que decida qué hacer. Es la diferencia con
// el bloque `tool` del grafo, donde un error salta por su propia rama.
func TestRunAgenticEntregaElErrorAlModelo(t *testing.T) {
	var second map[string]any
	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn++
		body, _ := io.ReadAll(r.Body)
		if turn == 2 {
			_ = json.Unmarshal(body, &second)
		}
		w.Header().Set("Content-Type", "application/json")
		if turn == 1 {
			_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","model":"M3",
				"content":[{"type":"tool_use","id":"tu_1","name":"search_data","input":{"query":"x"}}],
				"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"m2","type":"message","role":"assistant","model":"M3",
			"content":[{"type":"tool_use","id":"tu_2","name":"select_flow_branch",
				"input":{"branch":"conversar","reply":"Ahora mismo no puedo consultarlo."}}],
			"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	exec := func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
		return "", fmt.Errorf("la base no responde")
	}
	a := New("sk-test", srv.URL, "M3")
	reply, branch, _, err := a.RunAgenticUsage(context.Background(), "instrucción",
		map[string]string{"input": "hola"}, []string{"conversar"}, nil, false,
		[]AgentTool{{Name: "search_data", Description: "Busca.", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}},
		}}}, exec)
	if err != nil {
		t.Fatalf("un fallo de herramienta no debe abortar el turno: %v", err)
	}
	if branch != "conversar" || reply == "" {
		t.Fatalf("el agente debería haber respondido igualmente: %q / %q", reply, branch)
	}

	messages, _ := second["messages"].([]any)
	last, _ := messages[len(messages)-1].(map[string]any)
	blocks, _ := last["content"].([]any)
	block, _ := blocks[0].(map[string]any)
	if block["is_error"] != true {
		t.Fatalf("el resultado debía ir marcado como error: %v", block)
	}
	if content, _ := json.Marshal(block["content"]); !strings.Contains(string(content), "la base no responde") {
		t.Fatalf("el modelo no recibió la causa: %s", content)
	}
}

// Reproduce el fallo real de producción: MiniMax devolvió **dos** llamadas en un
// mismo mensaje y el bucle solo contestaba la primera. La segunda quedaba
// huérfana y la petición siguiente moría con
// «invalid params, tool call result does not follow tool call (2013)».
//
// `disable_parallel_tool_use` pide una sola llamada, pero el proveedor no está
// obligado a respetarlo: el protocolo exige un resultado por llamada y eso es lo
// que se comprueba aquí.
func TestRunAgenticRespondeTodasLasLlamadasDeUnMensaje(t *testing.T) {
	var second map[string]any
	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn++
		body, _ := io.ReadAll(r.Body)
		if turn == 2 {
			_ = json.Unmarshal(body, &second)
		}
		w.Header().Set("Content-Type", "application/json")
		if turn == 1 {
			_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","model":"M3",
				"content":[
					{"type":"tool_use","id":"tu_a","name":"search_data","input":{"query":"web"}},
					{"type":"tool_use","id":"tu_b","name":"search_data","input":{"query":"iot"}}
				],
				"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"m2","type":"message","role":"assistant","model":"M3",
			"content":[{"type":"tool_use","id":"tu_c","name":"select_flow_branch",
				"input":{"branch":"conversar","reply":"Hacemos webs y proyectos IoT."}}],
			"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer srv.Close()

	var queries []string
	exec := func(_ context.Context, _ string, input json.RawMessage) (string, error) {
		var args struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(input, &args)
		queries = append(queries, args.Query)
		return "resultado de " + args.Query, nil
	}

	a := New("sk-test", srv.URL, "M3")
	reply, branch, _, err := a.RunAgenticUsage(context.Background(), "instrucción",
		map[string]string{"input": "qué servicios ofreces"}, []string{"conversar", "asesor"}, nil, false,
		[]AgentTool{{Name: "search_data", Description: "Busca.", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}},
		}}}, exec)
	if err != nil {
		t.Fatalf("RunAgenticUsage: %v", err)
	}
	if branch != "conversar" || reply == "" {
		t.Fatalf("salida inesperada: %q / %q", reply, branch)
	}
	if len(queries) != 2 || queries[0] != "web" || queries[1] != "iot" {
		t.Fatalf("no se ejecutaron ambas llamadas: %v", queries)
	}

	// Lo que importa: un solo mensaje de resultados, con un bloque por llamada y
	// en el mismo orden en que el modelo las pidió.
	messages, _ := second["messages"].([]any)
	last, _ := messages[len(messages)-1].(map[string]any)
	blocks, _ := last["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("se esperaban 2 tool_result, hay %d", len(blocks))
	}
	ids := make([]string, 0, 2)
	for _, block := range blocks {
		b, _ := block.(map[string]any)
		if b["type"] != "tool_result" {
			t.Fatalf("bloque inesperado: %v", b["type"])
		}
		ids = append(ids, fmt.Sprint(b["tool_use_id"]))
	}
	if ids[0] != "tu_a" || ids[1] != "tu_b" {
		t.Fatalf("resultados mal atados o desordenados: %v", ids)
	}
}
