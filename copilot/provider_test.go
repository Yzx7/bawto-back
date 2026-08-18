package copilot

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

// sseMessage devuelve un turno completo del proveedor en formato SSE, con un
// bloque de razonamiento, uno de texto y una tool call. Es lo mínimo para que
// Accumulate reconstruya el mensaje y para que el sink de deltas reciba algo.
func sseMessage() string {
	events := []string{
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reviso el grafo"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Voy a leerlo"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_1","name":"get_flow_outline","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`,
		`{"type":"message_stop"}`,
	}
	var sb strings.Builder
	for _, event := range events {
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(event), &probe)
		fmt.Fprintf(&sb, "event: %s\ndata: %s\n\n", probe.Type, event)
	}
	return sb.String()
}

// sseEvents envuelve eventos sueltos en un cuerpo SSE.
func sseEvents(events ...string) string {
	var sb strings.Builder
	for _, event := range events {
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(event), &probe)
		fmt.Fprintf(&sb, "event: %s\ndata: %s\n\n", probe.Type, event)
	}
	return sb.String()
}

// providerServing levanta un endpoint falso que devuelve el cuerpo SSE dado.
func providerServing(t *testing.T, body string) *AnthropicProvider {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return NewAnthropicProvider("k", server.URL, "deepseek", "deepseek-v4-flash", "high", Rates{})
}

// providerWithCapture levanta un endpoint falso compatible con Anthropic y
// devuelve el provider junto al cuerpo de la última petición recibida.
func providerWithCapture(t *testing.T) (*AnthropicProvider, *map[string]any) {
	t.Helper()
	captured := map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		for key := range captured {
			delete(captured, key)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Errorf("cuerpo no era JSON: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseMessage())
	}))
	t.Cleanup(server.Close)
	return NewAnthropicProvider("k", server.URL, "deepseek", "deepseek-v4-flash", "high", Rates{}), &captured
}

// Los dos turnos que reventaron en local el 2026-08-17 al estrenar el
// streaming. Ninguno se daba con Messages.New, porque allí el cuerpo llegaba
// entero y ya parseado; acumulando el stream, el `input` de una tool call puede
// quedar vacío o duplicado y el SDK muere al marshalizar el bloque.
func TestNextReparaElInputDeLasToolCalls(t *testing.T) {
	const start = `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`
	const stop = `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`
	const end = `{"type":"message_stop"}`

	casos := []struct {
		nombre    string
		eventos   []string
		esperado  string
		reproduce string
	}{
		{
			nombre: "tool sin argumentos",
			eventos: []string{start,
				// Sin campo `input` y sin un solo input_json_delta: es lo que hace
				// una tool de catálogo, que no recibe parámetros.
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c1","name":"list_playbooks"}}`,
				`{"type":"content_block_stop","index":0}`, stop, end},
			esperado:  "{}",
			reproduce: "unexpected end of JSON input",
		},
		{
			nombre: "input completo en el start y repetido como delta",
			eventos: []string{start,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c2","name":"get_nodes","input":{"ids":["n1"]}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"ids\":[\"n1\"]}"}}`,
				`{"type":"content_block_stop","index":0}`, stop, end},
			esperado:  `{"ids":["n1"]}`,
			reproduce: "invalid character '{' after top-level value",
		},
		{
			nombre: "deltas que ni concatenados forman JSON válido",
			eventos: []string{start,
				// La llave de apertura llega suelta y luego el objeto entero: la
				// concatenación da `{{"ids":["n1"]}`, que es literalmente el
				// «invalid character '{' looking for beginning of object key
				// string» visto en local. Aquí los deltas no sirven y hay que
				// caer al input del start, no al objeto vacío: un get_nodes sin
				// ids no es la llamada que el modelo pidió.
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c3","name":"get_nodes","input":{"ids":["n1"]}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"ids\":[\"n1\"]}"}}`,
				`{"type":"content_block_stop","index":0}`, stop, end},
			esperado:  `{"ids":["n1"]}`,
			reproduce: "invalid character '{' looking for beginning of object key string",
		},
	}

	for _, caso := range casos {
		t.Run(caso.nombre, func(t *testing.T) {
			provider := providerServing(t, sseEvents(caso.eventos...))
			response, err := provider.Next(context.Background(), ModelRequest{
				SystemPrompt: "sistema",
				Initial:      &InitialModelContext{UserRequest: "x"},
				Tools:        AuthoringFunctionDefinitions(),
				Step:         1,
			})
			if err != nil {
				t.Fatalf("el turno murió (%s): %v", caso.reproduce, err)
			}
			if len(response.Calls) != 1 {
				t.Fatalf("se esperaba una tool call, hubo %d", len(response.Calls))
			}
			if got := string(response.Calls[0].Arguments); got != caso.esperado {
				t.Errorf("argumentos %s, esperado %s", got, caso.esperado)
			}
			if !json.Valid(response.Calls[0].Arguments) {
				t.Errorf("los argumentos deben ser JSON válido, fueron %q", response.Calls[0].Arguments)
			}
		})
	}
}

func toolChoiceType(t *testing.T, captured map[string]any) string {
	t.Helper()
	choice, ok := captured["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("la petición no llevaba tool_choice: %v", captured)
	}
	kind, _ := choice["type"].(string)
	return kind
}

// El paso terminal es el único que fuerza una tool por nombre, y forzar una
// tool por nombre es lo único que el modo razonamiento rechaza. Si alguien
// vuelve a apagar el thinking en todos los pasos —o lo deja encendido en el
// terminal— esta prueba lo detecta antes que un 400 en producción.
func TestSoloElPasoTerminalApagaElRazonamiento(t *testing.T) {
	provider, captured := providerWithCapture(t)
	request := ModelRequest{
		SystemPrompt: "sistema",
		Initial:      &InitialModelContext{UserRequest: "revisa el flujo"},
		Tools:        AuthoringFunctionDefinitions(),
		Step:         1,
	}

	if _, err := provider.Next(context.Background(), request); err != nil {
		t.Fatalf("paso de diseño: %v", err)
	}
	if kind := toolChoiceType(t, *captured); kind != "any" {
		t.Errorf("un paso de diseño debe usar tool_choice any, usó %q", kind)
	}
	if thinking, present := (*captured)["thinking"]; present {
		t.Errorf("un paso de diseño no debe apagar el razonamiento, mandó thinking=%v", thinking)
	}

	request.RequireTerminal = true
	request.Continuation = nil
	if _, err := provider.Next(context.Background(), request); err != nil {
		t.Fatalf("paso terminal: %v", err)
	}
	if kind := toolChoiceType(t, *captured); kind != "tool" {
		t.Errorf("el paso terminal debe forzar submit_proposal, usó tool_choice %q", kind)
	}
	thinking, ok := (*captured)["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("el paso terminal debe mandar thinking explícito, mandó %v", (*captured)["thinking"])
	}
	if thinking["type"] != "disabled" {
		t.Errorf("el paso terminal debe apagar el razonamiento, mandó %v", thinking["type"])
	}
}

// El razonamiento tiene que salir mientras el proveedor genera. Si el provider
// vuelve a Messages.New, no habrá un solo delta y el panel volverá a quedarse
// con la caja seca de actividades que motivó este cambio.
func TestNextEmiteDeltasDeRazonamientoYTexto(t *testing.T) {
	provider, _ := providerWithCapture(t)
	var deltas []StreamDelta
	ctx := WithDeltaSink(context.Background(), func(delta StreamDelta) {
		deltas = append(deltas, delta)
	})

	response, err := provider.Next(ctx, ModelRequest{
		SystemPrompt: "sistema",
		Initial:      &InitialModelContext{UserRequest: "revisa el flujo"},
		Tools:        AuthoringFunctionDefinitions(),
		Step:         3,
	})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}

	seen := map[string]string{}
	for _, delta := range deltas {
		if delta.Step != 3 {
			t.Errorf("delta atribuido al paso %d en vez del 3", delta.Step)
		}
		seen[delta.Kind] += delta.Content + delta.ToolName
	}
	if seen["thinking"] != "reviso el grafo" {
		t.Errorf("razonamiento en vivo esperado, llegó %q", seen["thinking"])
	}
	if seen["text"] != "Voy a leerlo" {
		t.Errorf("narración en vivo esperada, llegó %q", seen["text"])
	}
	if seen["tool"] != "get_flow_outline" {
		t.Errorf("el nombre de la tool debe anunciarse al abrir su bloque, llegó %q", seen["tool"])
	}
	// Los argumentos de la function call no pueden salir por el sink.
	for _, delta := range deltas {
		if strings.Contains(delta.Content, "partial_json") || delta.Content == "{}" {
			t.Errorf("un delta filtró argumentos de la tool: %+v", delta)
		}
	}
	if response.Thought != "reviso el grafo" {
		t.Errorf("el razonamiento acumulado debe persistirse, dio %q", response.Thought)
	}
	if len(response.Calls) != 1 || response.Calls[0].Name != "get_flow_outline" {
		t.Errorf("la tool call debe reconstruirse desde el stream, dio %+v", response.Calls)
	}
}

func TestToolParamsForMapsDefinitions(t *testing.T) {
	defs := AuthoringFunctionDefinitions()
	params := toolParamsFor(defs)
	if len(params) != len(defs) {
		t.Fatalf("toolParamsFor devolvió %d tools para %d definiciones", len(params), len(defs))
	}
	for i, param := range params {
		if param.OfTool == nil {
			t.Fatalf("tool %d sin OfTool", i)
		}
		if param.OfTool.Name != defs[i].Name {
			t.Fatalf("tool %d: nombre %q, esperado %q", i, param.OfTool.Name, defs[i].Name)
		}
		if param.OfTool.InputSchema.ExtraFields["additionalProperties"] != false {
			t.Fatalf("tool %s debe cerrar additionalProperties", defs[i].Name)
		}
	}
}

func TestInitialPromptIncludesRequest(t *testing.T) {
	initial := &InitialModelContext{UserRequest: "crea un flujo de cobros"}
	prompt := initialPrompt(initial)
	if !strings.Contains(prompt, "crea un flujo de cobros") {
		t.Fatalf("el prompt inicial debe incluir la petición del autor: %q", prompt)
	}
	if initialPrompt(nil) == "" {
		t.Fatal("un contexto nulo no debe producir un prompt vacío")
	}
}

func TestReasoningRequestOptions(t *testing.T) {
	// DeepSeek razona por defecto. Esta prueba exigía lo contrario hasta el
	// 2026-08-17: el proveedor apagaba el razonamiento del Copilot entero por un
	// 400 que solo ocurre al forzar UNA tool por nombre, cosa que únicamente
	// hace el paso terminal. Con thinking apagado en todos los pasos el panel no
	// tenía nada que mostrar, que es el fallo que se estaba arreglando.
	// Medido con cmd/copilotthinkprobe: `tool_choice: any` + thinking devuelve
	// bloques thinking sin error, en v4-flash y en v4-pro.
	if got := reasoningRequestOptions("deepseek", "high"); len(got) != 0 {
		t.Errorf("deepseek razona por defecto y no debe inyectar nada, dio %d opciones", len(got))
	}
	if got := reasoningRequestOptions("minimax", "high"); len(got) != 0 {
		t.Errorf("minimax razona por defecto y no debe inyectar nada, dio %d opciones", len(got))
	}
	if got := reasoningRequestOptions("anthropic", "high"); len(got) != 1 {
		t.Errorf("anthropic con effort high debe activar thinking, dio %d opciones", len(got))
	}
	if got := reasoningRequestOptions("anthropic", "none"); len(got) != 0 {
		t.Errorf("anthropic con effort none no debe activar thinking, dio %d opciones", len(got))
	}
}
