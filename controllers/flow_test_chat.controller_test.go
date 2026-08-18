package controllers

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/engine"
	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
)

// El chat de prueba existe para que el autor recorra el camino bueno de su
// flujo. Una herramienta que devolviera un error mandaría el grafo por la rama
// `error` y probaría justo lo contrario, así que la escritura simulada tiene que
// salir por `ok` y dejar sus variables en el estado.
func TestEscrituraSimuladaContinuaPorOkYNoPorError(t *testing.T) {
	flow := &engine.Flow{
		Trigger: engine.Trigger{Type: "message"},
		Nodes: []engine.Node{
			{ID: "n_save", Kind: "tool", ToolRef: "data_mutate", SaveAs: "payment_record", Args: map[string]string{
				"object": "cobros", "operation": "create", "field.estado": "needs_review", "field.monto": "35.5",
			}},
			{ID: "n_ok", Kind: "send", Body: "guardado {payment_record.recordId} estado={payment_record.data.estado}"},
			{ID: "n_error", Kind: "send", Body: "no se pudo guardar"},
		},
		Edges: []engine.Edge{
			{Source: "trigger", Target: "n_save"},
			{Source: "n_save", SourceHandle: "ok", Target: "n_ok"},
			{Source: "n_save", SourceHandle: "error", Target: "n_error"},
		},
	}
	run := &flowTestRun{}
	result, err := engine.Advance(flow, nil, "hola", engine.Deps{
		Tool: func(ref string, args, _ map[string]string) (string, error) {
			return run.simulateGraphTool(ref, args)
		},
	})
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if len(result.Sends) != 1 {
		t.Fatalf("sends inesperados: %#v", result.Sends)
	}
	if strings.Contains(result.Sends[0], "no se pudo guardar") {
		t.Fatalf("la escritura simulada salió por la rama error: %q", result.Sends[0])
	}
	if !strings.Contains(result.Sends[0], "estado=needs_review") {
		t.Fatalf("el grafo no recibió data.<campo> de la escritura simulada: %q", result.Sends[0])
	}
	if strings.Contains(result.Sends[0], "{payment_record.recordId}") {
		t.Fatalf("la escritura simulada no expuso recordId: %q", result.Sends[0])
	}
	if len(run.calls) != 1 || run.calls[0].Ref != "data_mutate" || !run.calls[0].Simulated {
		t.Fatalf("la llamada no quedó registrada como simulada: %#v", run.calls)
	}
	// El nodo no viaja: `Deps.Tool` recibe (ref, args, vars) y el motor no le pasa
	// el bloque. Anunciar un nodeId aquí sería adivinarlo casando argumentos.
	if run.calls[0].Source != "graph" || run.calls[0].NodeID != "" {
		t.Fatalf("una llamada del grafo no puede declarar nodo: %#v", run.calls[0])
	}
	if strings.TrimSpace(run.calls[0].Note) == "" || run.calls[0].Args["object"] != "cobros" {
		t.Fatalf("la llamada registrada no explica qué habría pasado: %#v", run.calls[0])
	}
}

// Cero coincidencias es `ok` con found=false (CLAUDE.md §10): la rama `error`
// queda para configuración inválida. Si la lectura simulada saliera por `error`,
// el autor no podría distinguir «no hay perfil» de «la lectura falló», que es
// precisamente la decisión que su Router tiene que tomar.
func TestLecturaSimuladaSalePorOkConFoundFalse(t *testing.T) {
	flow := &engine.Flow{
		Trigger: engine.Trigger{Type: "message"},
		Nodes: []engine.Node{
			{ID: "n_read", Kind: "tool", ToolRef: "data_query", SaveAs: "perfil", Args: map[string]string{"object": "perfiles_contacto"}},
			{ID: "n_router", Kind: "router", Cases: []engine.RouterCase{{ID: "con_perfil", Expression: "perfil.found == true"}}},
			{ID: "n_con", Kind: "send", Body: "con perfil"},
			{ID: "n_sin", Kind: "send", Body: "sin perfil"},
			{ID: "n_error", Kind: "send", Body: "la lectura falló"},
		},
		Edges: []engine.Edge{
			{Source: "trigger", Target: "n_read"},
			{Source: "n_read", SourceHandle: "ok", Target: "n_router"},
			{Source: "n_read", SourceHandle: "error", Target: "n_error"},
			{Source: "n_router", SourceHandle: "con_perfil", Target: "n_con"},
			{Source: "n_router", SourceHandle: "default", Target: "n_sin"},
		},
	}
	run := &flowTestRun{}
	result, err := engine.Advance(flow, nil, "hola", engine.Deps{
		Tool: func(ref string, args, _ map[string]string) (string, error) {
			return run.simulateGraphTool(ref, args)
		},
	})
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if len(result.Sends) != 1 || result.Sends[0] != "sin perfil" {
		t.Fatalf("la lectura simulada no salió por ok/found=false: %#v", result.Sends)
	}
	if !run.emptyReads {
		t.Fatal("la lectura vacía no quedó marcada para avisar al autor")
	}
}

// La simulación tiene que espejar la forma del ejecutor real. Se comprueba
// decodificando en la struct del ejecutor con DisallowUnknownFields: un campo
// inventado o renombrado pone la prueba roja, que es el fallo que el autor
// vería como «el grafo lee la variable y sale vacía».
func TestFormaDeCadaHerramientaSimuladaEspejaLaReal(t *testing.T) {
	casos := []struct {
		ref    string
		args   map[string]string
		target any
	}{
		{"data_mutate", map[string]string{"object": "cobros", "operation": "create", "field.estado": "x"}, &models.DataMutationResult{}},
		{"data_query", map[string]string{"object": "cobros"}, &models.DataQueryResult{}},
		{"catalog_search", map[string]string{"connection": "meudim"}, &catalogResult{}},
		{"catalog_product", map[string]string{"connection": "meudim", "productId": "7"}, &catalogResult{}},
		{"dataset_query", map[string]string{"connection": "ds", "resource": "clientes"}, &datasetResult{}},
		{"order_create", map[string]string{"connection": "meudim", "item.1.productId": "7", "item.1.quantity": "2", "currency": "PEN"}, &orderResult{}},
		{"payment_intent_create", map[string]string{"connection": "meudim", "orderId": "12", "provider": "yape"}, &paymentIntentResult{}},
		{"payment_submit", map[string]string{"connection": "meudim", "paymentId": "9", "reference": "00123"}, &paymentSubmitResult{}},
		{"payment_methods_render", map[string]string{}, &models.PaymentInstructionsResult{}},
		{"subscription_activate", map[string]string{"activationCode": "ABC", "planKey": "pro", "billingCycle": "monthly"}, &models.OrganizationSubscription{}},
		{"credit_recharge_activate", map[string]string{"activationCode": "ABC"}, &models.CreditWallet{}},
	}
	for _, caso := range casos {
		run := &flowTestRun{}
		raw, note, err := flowTestGraphToolResult(caso.ref, caso.args, run)
		if err != nil {
			t.Fatalf("%s: la simulación devolvió error y mandaría el flujo por la rama equivocada: %v", caso.ref, err)
		}
		if strings.TrimSpace(note) == "" {
			t.Fatalf("%s: la simulación no explica qué habría pasado de verdad", caso.ref)
		}
		decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(caso.target); err != nil {
			t.Fatalf("%s: la forma simulada no es la del ejecutor real: %v (%s)", caso.ref, err, raw)
		}
	}

	// Los campos que el grafo consulta por nombre se comprueban uno a uno: que la
	// struct decodifique no garantiza que estén poblados.
	run := &flowTestRun{}
	raw, _, err := flowTestGraphToolResult("data_mutate", map[string]string{
		"object": "cobros", "operation": "upsert", "field.estado": "aceptado",
	}, run)
	if err != nil {
		t.Fatalf("data_mutate: %v", err)
	}
	var mutation models.DataMutationResult
	if err := json.Unmarshal([]byte(raw), &mutation); err != nil {
		t.Fatalf("data_mutate: %v", err)
	}
	if mutation.RecordID == "" || mutation.ObjectKey != "cobros" || mutation.Operation != "upsert" || !mutation.Created {
		t.Fatalf("data_mutate simulado incompleto: %+v", mutation)
	}
	if string(mutation.Data) != `{"estado":"aceptado"}` {
		t.Fatalf("data_mutate simulado no proyectó los campos del bloque: %s", mutation.Data)
	}

	raw, _, err = flowTestGraphToolResult("data_query", map[string]string{"object": "segmentos"}, run)
	if err != nil {
		t.Fatalf("data_query: %v", err)
	}
	var query models.DataQueryResult
	if err := json.Unmarshal([]byte(raw), &query); err != nil {
		t.Fatalf("data_query: %v", err)
	}
	if query.Found || query.Count != 0 || query.First != nil || query.Records == nil {
		t.Fatalf("data_query simulado no es una lectura vacía válida: %+v", query)
	}
}

// Lo que ve el modelo en el lado del agente es texto, no JSON, y tiene que ser
// **el mismo** texto que produce el ejecutor real sin resultados. Si divergen,
// el autor afinaría el prompt contra una frase que producción nunca envía.
func TestHerramientasDeAgenteSimuladasDevuelvenElTextoReal(t *testing.T) {
	result, note := flowTestAgentToolResult("data_query", map[string]string{"object": "planes_bawto"})
	if result != dataQueryEmptyForModel([]string{"planes_bawto"}) {
		t.Fatalf("data_query de agente no espeja el texto real: %q", result)
	}
	if strings.TrimSpace(note) == "" {
		t.Fatal("data_query de agente no explica la simulación")
	}
	if got, _ := flowTestAgentToolResult("catalog_search", nil); got != catalogSearchEmptyForModel {
		t.Fatalf("catalog_search de agente no espeja el texto real: %q", got)
	}
	if got, _ := flowTestAgentToolResult("catalog_product", nil); got != catalogProductMissingForModel {
		t.Fatalf("catalog_product de agente no espeja el texto real: %q", got)
	}
	if got, _ := flowTestAgentToolResult("dataset_query", nil); got != datasetEmptyForModel {
		t.Fatalf("dataset_query de agente no espeja el texto real: %q", got)
	}

	// Comparar contra el mismo símbolo no probaría nada por sí solo: si alguien
	// volviera a incrustar el literal dentro del ejecutor real, las dos frases se
	// separarían y esta prueba seguiría verde. Se comprueba sobre el fuente que
	// **ambos lados** siguen usando el símbolo compartido.
	usos := map[string]map[string]bool{}
	for _, archivo := range []string{
		"agent_tools.go", "catalog_tools.go", "dataset_tools.go", "flow_test_chat.controller.go",
	} {
		file, err := parser.ParseFile(token.NewFileSet(), archivo, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", archivo, err)
		}
		for _, decl := range file.Decls {
			funcDecl, ok := decl.(*ast.FuncDecl)
			if !ok || funcDecl.Body == nil {
				continue
			}
			nombres := map[string]bool{}
			ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
				if ident, ok := node.(*ast.Ident); ok {
					nombres[ident.Name] = true
				}
				return true
			})
			usos[funcDecl.Name.Name] = nombres
		}
	}
	compartidos := map[string]string{
		"execAgentDataQuery":      "dataQueryEmptyForModel",
		"execAgentCatalogSearch":  "catalogSearchEmptyForModel",
		"execAgentCatalogProduct": "catalogProductMissingForModel",
		"execAgentDatasetQuery":   "datasetEmptyForModel",
	}
	for ejecutor, simbolo := range compartidos {
		if !usos[ejecutor][simbolo] {
			t.Errorf("%s dejó de usar %s: el modelo leería un texto distinto en producción que en la prueba",
				ejecutor, simbolo)
		}
		if !usos["flowTestAgentToolResult"][simbolo] {
			t.Errorf("la simulación dejó de usar %s", simbolo)
		}
	}
}

// Los ejecutores reales escriben en Data, cierran pedidos y mueven dinero y
// créditos. Ninguno puede alcanzarse desde el chat de prueba, y una prueba de
// comportamiento no lo demostraría: bastaría con que la rama no se recorriera en
// ese caso concreto. Se comprueba sobre el propio fuente.
func TestElChatDePruebaNoInvocaNingunEjecutorReal(t *testing.T) {
	prohibidos := map[string]bool{
		"execDataMutate": true, "execDataQuery": true, "execCatalogSearch": true,
		"execCatalogProduct": true, "execDatasetQuery": true, "execOrderCreate": true,
		"execPaymentIntentCreate": true, "execPaymentSubmit": true,
		"execPaymentMethodsRender": true, "execSubscriptionActivate": true,
		"execCreditRechargeActivate": true, "execAgentDataQuery": true,
		"execAgentCatalogSearch": true, "execAgentCatalogProduct": true,
		"execAgentDatasetQuery": true, "agentTooling": true,
		// Las primitivas de modelo detrás de esos ejecutores, por si alguien las
		// llamara sin pasar por ellos.
		"MutateDataRecord": true, "QueryDataRecords": true,
		"RechargePlatformCredits": true, "ActivatePlatformSubscription": true,
		"PaymentInstructionsForOrg": true,
	}
	file, err := parser.ParseFile(token.NewFileSet(), "flow_test_chat.controller.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			name = fun.Name
		case *ast.SelectorExpr:
			name = fun.Sel.Name
		}
		if prohibidos[name] {
			t.Errorf("el chat de prueba invoca un ejecutor real: %s", name)
		}
		return true
	})
}

// Un estado de otro flujo trae nodeId y variables que aquí significan otra cosa.
// Reanudarlo produciría un recorrido inexplicable en vez de un error claro.
func TestEstadoDeOtroFlujoSeRechaza(t *testing.T) {
	raw, err := json.Marshal(engine.State{FlowID: "otro", NodeID: "n_wait"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, problem := parseFlowTestState(raw, "propio"); problem == "" {
		t.Fatal("se aceptó el estado de otro flujo")
	}
	propio, err := json.Marshal(engine.State{FlowID: "propio", NodeID: "n_wait"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	state, problem := parseFlowTestState(propio, "propio")
	if problem != "" || state == nil || state.NodeID != "n_wait" {
		t.Fatalf("estado propio rechazado: %q", problem)
	}
	for _, vacio := range []string{"", "null", "{}"} {
		if state, problem := parseFlowTestState(json.RawMessage(vacio), "propio"); state != nil || problem != "" {
			t.Fatalf("%q no arrancó de cero: state=%v problem=%q", vacio, state, problem)
		}
	}
}

// Sin contacto, un flujo que lee el perfil se va siempre por «sin perfil». Es
// aceptable en este alcance, pero tiene que decirse: en silencio, el autor
// concluiría que su Router está mal.
//
// El aviso es de ámbito `session` porque se repetiría idéntico en cada turno; el
// panel lo enseña una vez en vez de ensuciar el hilo.
func TestSeAvisaDeLaFaltaDeContactoUnaSolaVez(t *testing.T) {
	sinContacto := &engine.Flow{Nodes: []engine.Node{{ID: "n", Kind: "send", Body: "hola"}}}
	if warnings := flowTestSessionWarnings(sinContacto); len(warnings) != 0 {
		t.Fatalf("avisos en un flujo que no los merece: %#v", warnings)
	}
	vinculado := &engine.Flow{Nodes: []engine.Node{
		{ID: "n", Kind: "tool", ToolRef: "data_query", Args: map[string]string{"object": "perfiles", "linkCurrentContact": "true"}},
	}}
	warnings := flowTestSessionWarnings(vinculado)
	if !contieneAviso(warnings, flowTestWarnNoContact, flowTestScopeSession) {
		t.Fatalf("no se avisó del vínculo con el contacto: %#v", warnings)
	}
	// Un flujo con bloques de herramienta avisa además de que nada se ejecuta.
	if !contieneAviso(warnings, flowTestWarnToolsSimulated, flowTestScopeSession) {
		t.Fatalf("no se avisó de que las herramientas se simulan: %#v", warnings)
	}
	interpolado := &engine.Flow{Nodes: []engine.Node{{ID: "n", Kind: "send", Body: "Hola {contact_name}"}}}
	if !contieneAviso(flowTestSessionWarnings(interpolado), flowTestWarnNoContact, flowTestScopeSession) {
		t.Fatalf("no se avisó del uso de variables de contacto")
	}
}

// Un aviso sin código obliga al panel a leer castellano para decidir cómo
// pintarlo, y sin ámbito no puede distinguir lo estructural de lo puntual.
func TestLosAvisosLlevanCodigoYAmbito(t *testing.T) {
	run := &flowTestRun{}
	run.warn(flowTestWarnNoSends, flowTestScopeTurn, "sin mensajes")
	run.warn(flowTestWarnNoSends, flowTestScopeTurn, "sin mensajes")
	if len(run.warnings) != 1 {
		t.Fatalf("el mismo aviso se repitió: %#v", run.warnings)
	}
	for _, warning := range run.warnings {
		if warning.Code == "" || warning.Message == "" {
			t.Fatalf("aviso incompleto: %#v", warning)
		}
		if warning.Scope != flowTestScopeSession && warning.Scope != flowTestScopeTurn {
			t.Fatalf("ámbito desconocido: %#v", warning)
		}
	}
}

func contieneAviso(warnings []flowTestWarning, code, scope string) bool {
	for _, warning := range warnings {
		if warning.Code == code && warning.Scope == scope {
			return true
		}
	}
	return false
}

// Sin código, el panel solo puede pintar una caja roja genérica: no sabe si
// ofrecer «recargar créditos» o señalar el bloque mal configurado. El de
// validación además tiene que llevar el problema concreto.
func TestLosErroresDelChatDePruebaLlevanCodigoEstable(t *testing.T) {
	app := fiber.New()
	con := &Controller{}
	app.Get("/creditos", func(c *fiber.Ctx) error {
		return con.failFlowTest(c, fiber.StatusPaymentRequired, flowTestErrCredits, "sin saldo", nil)
	})
	app.Get("/borrador", func(c *fiber.Ctx) error {
		return con.failFlowTest(c, fiber.StatusBadRequest, flowTestErrDraft,
			"el borrador no es válido: el nodo n_x no tiene salida",
			map[string]any{"problem": "el nodo n_x no tiene salida"})
	})

	response, err := app.Test(httptest.NewRequest("GET", "/creditos", nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != fiber.StatusPaymentRequired {
		t.Fatalf("status=%d", response.StatusCode)
	}
	var envelope types.GenRes
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	data, _ := envelope.Data.(map[string]any)
	if envelope.Ok || data["code"] != flowTestErrCredits {
		t.Fatalf("saldo agotado no llega distinguible: %+v", envelope)
	}

	draftResponse, err := app.Test(httptest.NewRequest("GET", "/borrador", nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer draftResponse.Body.Close()
	if draftResponse.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status=%d", draftResponse.StatusCode)
	}
	var draftEnvelope types.GenRes
	if err := json.NewDecoder(draftResponse.Body).Decode(&draftEnvelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	draftData, _ := draftEnvelope.Data.(map[string]any)
	if draftData["code"] != flowTestErrDraft || draftData["problem"] != "el nodo n_x no tiene salida" {
		t.Fatalf("la validación no transporta el problema concreto: %+v", draftEnvelope)
	}
}

func TestSeAvisaDeLosMarcadoresSinResolver(t *testing.T) {
	markers := flowTestUnresolvedMarkers("Debes {receipt.amount} soles, {receipt.amount} en total, gracias")
	if len(markers) != 1 || markers[0] != "{receipt.amount}" {
		t.Fatalf("marcadores mal detectados: %#v", markers)
	}
	if len(flowTestUnresolvedMarkers("todo resuelto")) != 0 {
		t.Fatal("se avisó de un texto sin marcadores")
	}
}
