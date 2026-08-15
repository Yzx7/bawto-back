package mcp

import (
	"context"

	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/authoring"
	"github.com/Yzx7/sacs-chatbots/engine"
	"github.com/Yzx7/sacs-chatbots/models"
)

// flujoValido es el grafo mínimo que engine.Validate acepta: un send necesita
// exactamente una salida y solo un action=end puede terminar. Lleva `pos`
// porque una de las pruebas comprueba que el campo desconocido por el motor
// sobrevive a la escritura.
const flujoValido = `{
  "id": "f_demo",
  "name": "Demo",
  "trigger": {"type": "message"},
  "nodes": [
    {"id": "n_saludo", "kind": "send", "body": "hola", "pos": {"x": 40, "y": 120}},
    {"id": "n_fin", "kind": "action", "action": "end", "pos": {"x": 320, "y": 120}}
  ],
  "edges": [
    {"id": "e_in", "source": "trigger", "target": "n_saludo"},
    {"id": "e_fin", "source": "n_saludo", "target": "n_fin"}
  ]
}`

const orgPropia = "11111111-1111-1111-1111-111111111111"
const orgAjena = "22222222-2222-2222-2222-222222222222"

type fakeStore struct {
	bots       map[string]*models.Bot
	flows      map[string]*models.Flow
	escrituras int
	// keys se indexa por la cadena presentada, como haría el índice sobre el
	// hash: la prueba no puede conocer secretos que el modelo no guarda.
	keys    map[string]*models.MCPKey
	tocadas int
	// devolverRevocadas simula un SELECT al que le hubieran quitado el filtro por
	// revocación: es la única forma de comprobar la defensa del servicio, porque
	// el SQL real no lo ejecuta ninguna prueba de este paquete.
	devolverRevocadas bool
}

// ResolveKey reproduce el filtro real: revocada o caducada resuelven a nil, sin
// distinguirse de una key que nunca existió.
func (f *fakeStore) ResolveKey(_ context.Context, presented string) (*models.MCPKey, error) {
	key := f.keys[presented]
	if key == nil || (!key.Active(time.Now()) && !f.devolverRevocadas) {
		return nil, nil
	}
	copia := *key
	return &copia, nil
}

func (f *fakeStore) TouchKey(_ context.Context, _ string) error {
	f.tocadas++
	return nil
}

func (f *fakeStore) ListBots(_ context.Context, orgID string) ([]models.Bot, error) {
	result := make([]models.Bot, 0)
	for _, bot := range f.bots {
		if bot.OrgID == orgID {
			result = append(result, *bot)
		}
	}
	return result, nil
}

func (f *fakeStore) GetBot(_ context.Context, botID string) (*models.Bot, error) {
	return f.bots[botID], nil
}

func (f *fakeStore) ListFlows(_ context.Context, botID string) ([]models.Flow, error) {
	result := make([]models.Flow, 0)
	for _, flow := range f.flows {
		if flow.BotID == botID {
			result = append(result, *flow)
		}
	}
	return result, nil
}

func (f *fakeStore) GetFlow(_ context.Context, botID, flowID string) (*models.Flow, error) {
	flow := f.flows[flowID]
	if flow == nil || flow.BotID != botID {
		return nil, nil
	}
	return flow, nil
}

// UpdateDraft reproduce el compare-and-swap de models.UpdateFlowDraft, incluido
// su no-op por contenido: sin eso, la prueba del conflicto pasaría por razones
// distintas a las del código real.
func (f *fakeStore) UpdateDraft(
	_ context.Context,
	botID, flowID string,
	draft json.RawMessage,
	expectedChecksum, userID string,
) (*models.DraftSnapshot, error) {
	flow := f.flows[flowID]
	if flow == nil || flow.BotID != botID {
		return nil, nil
	}
	current, err := models.DraftSnapshotFromFlow(flow)
	if err != nil {
		return nil, err
	}
	canonical, candidate, err := engine.CanonicalChecksum(draft)
	if err != nil {
		return nil, err
	}
	if candidate == current.Checksum {
		return current, nil
	}
	if expectedChecksum != current.Checksum {
		return nil, &models.DraftConflictError{
			Code: "draft_conflict", ExpectedChecksum: expectedChecksum,
			CurrentChecksum: current.Checksum, CurrentDraft: current.Draft,
			CurrentUpdatedAt: current.UpdatedAt,
		}
	}
	f.escrituras++
	flow.Draft = canonical
	flow.UpdatedBy = &userID
	return models.DraftSnapshotFromFlow(flow)
}

func (f *fakeStore) Resources(_ context.Context, _, _ string) (authoring.AuthoringResourceSnapshot, error) {
	return authoring.AuthoringResourceSnapshot{}, nil
}

func nuevoServidor() (*session, *fakeStore) {
	store := &fakeStore{
		bots: map[string]*models.Bot{
			"bot-propio":  {ID: "bot-propio", OrgID: orgPropia, Name: "Propio", Channel: "wsp"},
			"bot-hermano": {ID: "bot-hermano", OrgID: orgPropia, Name: "Hermano", Channel: "wsp"},
			"bot-ajeno":   {ID: "bot-ajeno", OrgID: orgAjena, Name: "Ajeno", Channel: "wsp"},
		},
		flows: map[string]*models.Flow{
			"flujo-1": {
				ID: "flujo-1", BotID: "bot-propio", Key: "principal", Name: "Principal",
				TriggerType: "message", Status: "draft", Draft: json.RawMessage(flujoValido),
				UpdatedAt: time.Now().UTC(),
			},
			// Mismo bot y misma organización que flujo-1: es el vecino que un
			// token acotado no debe alcanzar. El aislamiento por organización no
			// lo tapa, así que solo el alcance puede.
			"flujo-2": {
				ID: "flujo-2", BotID: "bot-propio", Key: "secundario", Name: "Secundario",
				TriggerType: "message", Status: "draft", Draft: json.RawMessage(flujoValido),
				UpdatedAt: time.Now().UTC(),
			},
			// Otro bot de la misma organización, entero fuera del alcance.
			"flujo-hermano": {
				ID: "flujo-hermano", BotID: "bot-hermano", Key: "hermano", Name: "Hermano",
				TriggerType: "message", Status: "draft", Draft: json.RawMessage(flujoValido),
			},
			"flujo-ajeno": {
				ID: "flujo-ajeno", BotID: "bot-ajeno", Key: "otro", Name: "Otro",
				TriggerType: "message", Status: "draft", Draft: json.RawMessage(flujoValido),
			},
		},
	}
	store.keys = map[string]*models.MCPKey{
		keyCompleta: {ID: "key-completa", OrgID: orgPropia, Name: "completa", CreatedBy: &creador},
		keyAcotada: {ID: "key-acotada", OrgID: orgPropia, Name: "acotada",
			FlowIDs: []string{"flujo-1"}, CreatedBy: &creador},
	}
	return &session{store: store, key: store.keys[keyCompleta]}, store
}

// Las cadenas de prueba llevan el prefijo real: el servicio lo exige antes de
// consultar, así que una key de prueba sin él probaría otro camino.
const (
	keyCompleta = models.MCPKeyPrefix + "completa000000000000000000000000000000000000"
	keyAcotada  = models.MCPKeyPrefix + "acotada0000000000000000000000000000000000000"
)

var creador = "user-1"

// --- arnés HTTP -------------------------------------------------------------

// appDePrueba monta el servicio sobre el fixture, exactamente como lo monta
// routes.RegisterHTTP. Un único Service atiende todas las peticiones, que es
// justo la condición que hace peligroso guardar identidad fuera de la petición.
func appDePrueba() (*fiber.App, *fakeStore) {
	_, store := nuevoServidor()
	service := &Service{store: store}
	app := fiber.New()
	service.Register(app)
	return app, store
}

func postMCP(t *testing.T, app *fiber.App, token, cuerpo string) *http.Response {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/mcp/flows", strings.NewReader(cuerpo))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := app.Test(request, -1)
	if err != nil {
		t.Fatalf("POST /mcp/flows: %v", err)
	}
	return response
}

func leerCuerpo(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("leer cuerpo: %v", err)
	}
	return string(raw)
}

// llamarHTTP ejecuta una tool por el transporte real y devuelve el payload.
func llamarHTTP(t *testing.T, app *fiber.App, token, name, arguments string) (map[string]any, bool) {
	t.Helper()
	cuerpo, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": json.RawMessage(arguments)},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	response := postMCP(t, app, token, string(cuerpo))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s respondió %d: %s", name, response.StatusCode, leerCuerpo(t, response))
	}
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	crudo := leerCuerpo(t, response)
	if err := json.Unmarshal([]byte(crudo), &envelope); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, crudo)
	}
	if len(envelope.Result.Content) != 1 {
		t.Fatalf("%s no devolvió contenido: %s", name, crudo)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &payload); err != nil {
		t.Fatalf("%s no devolvió JSON: %v", name, err)
	}
	return payload, envelope.Result.IsError
}

// servidorAcotado usa la misma base con la key que solo alcanza flujo-1.
func servidorAcotado() (*session, *fakeStore) {
	_, store := nuevoServidor()
	return &session{store: store, key: store.keys[keyAcotada]}, store
}

// llamar ejecuta una tool como lo haría el cliente y devuelve el JSON del
// contenido más si vino marcado como error para el modelo.
func llamar(t *testing.T, srv *session, name string, arguments string) (map[string]any, bool) {
	t.Helper()
	params, err := json.Marshal(map[string]any{"name": name, "arguments": json.RawMessage(arguments)})
	if err != nil {
		t.Fatalf("params: %v", err)
	}
	raw, err := srv.dispatch(context.Background(), "tools/call", params)
	if err != nil {
		t.Fatalf("%s devolvió un error de protocolo: %v", name, err)
	}
	result, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%s no devolvió un resultado de tool", name)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("%s no devolvió contenido", name)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("%s no devolvió JSON: %v\n%s", name, err, text)
	}
	isError, _ := result["isError"].(bool)
	return payload, isError
}

func TestHandshakeYListadoDeHerramientas(t *testing.T) {
	app, store := appDePrueba()
	_ = store

	// La notificación no lleva id: el protocolo prohíbe contestarla y el
	// transporte HTTP lo expresa con un 202 sin cuerpo.
	notificacion := postMCP(t, app, keyCompleta,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if notificacion.StatusCode != http.StatusAccepted {
		t.Fatalf("una notificación debe responder 202 y respondió %d", notificacion.StatusCode)
	}
	if cuerpo := leerCuerpo(t, notificacion); strings.TrimSpace(cuerpo) != "" {
		t.Fatalf("una notificación no lleva cuerpo: %q", cuerpo)
	}

	var handshake struct {
		Result struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
		} `json:"result"`
	}
	respuesta := leerCuerpo(t, postMCP(t, app, keyCompleta,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"prueba","version":"1"}}}`))
	if err := json.Unmarshal([]byte(respuesta), &handshake); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if handshake.Result.ProtocolVersion != "2024-11-05" {
		t.Fatalf("el servidor no acompañó la versión del cliente: %s", handshake.Result.ProtocolVersion)
	}
	if _, ok := handshake.Result.Capabilities["tools"]; !ok {
		t.Fatalf("el handshake no anuncia tools")
	}

	var listado struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	catalogo := leerCuerpo(t, postMCP(t, app, keyCompleta, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	if err := json.Unmarshal([]byte(catalogo), &listado); err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	nombres := map[string]bool{}
	for _, tool := range listado.Result.Tools {
		nombres[tool.Name] = true
		if tool.Description == "" {
			t.Fatalf("%s sin descripción", tool.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("%s tiene un inputSchema que no es JSON: %v", tool.Name, err)
		}
	}
	// Cuatro y solo cuatro: cualquier quinta capacidad tendría que justificarse
	// contra el §4, y una que publicara rompería el invariante del borrador.
	if len(nombres) != 4 || !nombres["flow_get"] || !nombres["flow_spec"] ||
		!nombres["flow_validate"] || !nombres["flow_put"] {
		t.Fatalf("el catálogo de tools no es el del §4: %v", nombres)
	}
	for name := range toolHandlers {
		if strings.Contains(name, "publish") || strings.Contains(name, "publicar") {
			t.Fatalf("existe una capacidad de publicación: %s", name)
		}
	}
}

func TestFlowSpecEntregaCatalogHashYReglas(t *testing.T) {
	srv, _ := nuevoServidor()
	payload, isError := llamar(t, srv, "flow_spec", `{}`)
	if isError {
		t.Fatalf("flow_spec falló: %v", payload)
	}
	if payload["catalogHash"] != authoring.CatalogHash() {
		t.Fatalf("catalogHash no viene del catálogo real: %v", payload["catalogHash"])
	}
	for _, clave := range []string{"nodeKinds", "runtimeTools", "playbooks", "rules"} {
		if payload[clave] == nil {
			t.Fatalf("flow_spec no entrega %s", clave)
		}
	}
	// Una sección parcial sigue llevando el hash: quien cachea solo los nodos
	// también necesita saber cuándo caducaron.
	parcial, _ := llamar(t, srv, "flow_spec", `{"section":"nodes"}`)
	if parcial["catalogHash"] != authoring.CatalogHash() || parcial["runtimeTools"] != nil {
		t.Fatalf("la sección nodes no respeta el contrato: %v", parcial)
	}
}

func TestFlowValidateNoEscribeYSeparaErroresDeAvisos(t *testing.T) {
	srv, store := nuevoServidor()
	bueno, isError := llamar(t, srv, "flow_validate",
		`{"flow": `+flujoValido+`}`)
	if isError || bueno["ok"] != true {
		t.Fatalf("un grafo válido no debería tener errores: %v", bueno)
	}
	if bueno["checksum"] == "" {
		t.Fatalf("flow_validate no devolvió el checksum del candidato")
	}
	if bueno["resourcesChecked"] != false {
		t.Fatalf("sin botId no se comprobaron recursos y hay que decirlo: %v", bueno)
	}

	// Un nodo huérfano: el motor lo rechaza y el modelo debe recibir el motivo,
	// no un fallo de protocolo.
	roto := `{"id":"f","name":"D","trigger":{"type":"message"},"nodes":[{"id":"a","kind":"send","body":"x"}],"edges":[]}`
	malo, _ := llamar(t, srv, "flow_validate", `{"flow": `+roto+`}`)
	if malo["ok"] != false {
		t.Fatalf("un grafo inválido pasó la validación: %v", malo)
	}
	if errores, _ := malo["errors"].(float64); errores < 1 {
		t.Fatalf("no se reportó ningún error: %v", malo)
	}
	if store.escrituras != 0 {
		t.Fatalf("flow_validate escribió en la base")
	}
}

func TestFlowValidateAceptaElFlujoComoTexto(t *testing.T) {
	srv, _ := nuevoServidor()
	comoTexto, err := json.Marshal(flujoValido)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	payload, isError := llamar(t, srv, "flow_validate", `{"flow": `+string(comoTexto)+`}`)
	if isError || payload["ok"] != true {
		t.Fatalf("un flujo serializado dentro de una cadena debería validarse igual: %v", payload)
	}
}

func TestFlowPutExigeChecksumYExplicaComoResolverElConflicto(t *testing.T) {
	srv, store := nuevoServidor()
	nuevo := strings.Replace(flujoValido, `"body": "hola"`, `"body": "buenas"`, 1)

	sinChecksum, isError := llamar(t, srv, "flow_put",
		`{"botId":"bot-propio","flowId":"flujo-1","flow":`+nuevo+`,"expectedChecksum":""}`)
	if !isError || sinChecksum["code"] != "missing_checksum" {
		t.Fatalf("escribir sin checksum debería fallar: %v", sinChecksum)
	}

	viejo, isError := llamar(t, srv, "flow_put",
		`{"botId":"bot-propio","flowId":"flujo-1","flow":`+nuevo+`,"expectedChecksum":"0000"}`)
	if !isError || viejo["code"] != "draft_conflict" {
		t.Fatalf("un checksum vencido debería dar draft_conflict: %v", viejo)
	}
	mensaje, _ := viejo["message"].(string)
	if !strings.Contains(mensaje, "flow_get") || !strings.Contains(mensaje, "fusiona") {
		t.Fatalf("el conflicto no dice que hay que reexportar y fusionar: %q", mensaje)
	}
	if store.escrituras != 0 {
		t.Fatalf("una escritura con checksum vencido llegó a la base")
	}

	lectura, _ := llamar(t, srv, "flow_get", `{"botId":"bot-propio","flowId":"flujo-1"}`)
	flow, _ := lectura["flow"].(map[string]any)
	checksum, _ := flow["draftChecksum"].(string)
	guardado, isError := llamar(t, srv, "flow_put",
		`{"botId":"bot-propio","flowId":"flujo-1","flow":`+nuevo+`,"expectedChecksum":"`+checksum+`"}`)
	if isError {
		t.Fatalf("con el checksum vigente debería guardar: %v", guardado)
	}
	if guardado["saved"] != true || guardado["published"] != false {
		t.Fatalf("flow_put debe guardar el borrador y no publicar: %v", guardado)
	}
	if store.escrituras != 1 {
		t.Fatalf("se esperaba exactamente una escritura y hubo %d", store.escrituras)
	}
	if store.flows["flujo-1"].PublishedVersionID != nil {
		t.Fatalf("flow_put publicó una versión")
	}
	// El `pos` de cada nodo no lo entiende el motor y aun así debe sobrevivir:
	// perderlo destruiría el layout que el dueño ordenó en el panel.
	if !strings.Contains(string(store.flows["flujo-1"].Draft), `"pos"`) {
		t.Fatalf("la escritura borró los campos que el motor no conoce")
	}
}

// Reenviar el mismo documento no es un conflicto: models resuelve el no-op por
// contenido antes de mirar el checksum, para que un reintento cuyo response se
// perdió sea idempotente. Lo que no puede pasar es que la respuesta lo cuente
// como una edición: el modelo se creería autor de un cambio inexistente y
// seguiría construyendo sobre esa creencia.
func TestFlowPutConDocumentoIdenticoNoEscribeNiDiceQueCambio(t *testing.T) {
	srv, store := nuevoServidor()
	payload, isError := llamar(t, srv, "flow_put",
		`{"botId":"bot-propio","flowId":"flujo-1","flow":`+flujoValido+`,"expectedChecksum":"vencido"}`)
	if isError {
		t.Fatalf("reenviar el mismo documento no debería fallar: %v", payload)
	}
	if payload["changed"] != false {
		t.Fatalf("una escritura sin cambio real se reportó como cambio: %v", payload)
	}
	if store.escrituras != 0 {
		t.Fatalf("se escribió un documento idéntico al vivo")
	}
}

// engineValid y ok responden preguntas distintas: publicable para el motor, y
// sin ninguna observación de autoría. El borrador vivo de Sistemuino pasa lo
// primero y falla lo segundo; fundirlas mandaría a una IA a reescribir flujos
// que el panel publica sin protestar.
func TestFlowValidateDistingueMotorDeAutoria(t *testing.T) {
	srv, _ := nuevoServidor()
	// Dos bloques que guardan en la misma variable: el motor lo acepta, la
	// autoría lo marca como colisión.
	colision := `{"id":"f","name":"D","trigger":{"type":"message"},
	  "nodes":[{"id":"a","kind":"action","action":"set","params":{"x":"1"}},
	           {"id":"b","kind":"action","action":"set","params":{"x":"2"}},
	           {"id":"z","kind":"action","action":"end"}],
	  "edges":[{"id":"e1","source":"trigger","target":"a"},
	           {"id":"e2","source":"a","target":"b"},
	           {"id":"e3","source":"b","target":"z"}]}`
	payload, _ := llamar(t, srv, "flow_validate", `{"flow": `+colision+`}`)
	if payload["engineValid"] != true {
		t.Fatalf("el motor debería aceptar este grafo: %v", payload)
	}
	if payload["ok"] != false {
		t.Fatalf("la autoría debería objetar la colisión de variable: %v", payload)
	}

	roto := `{"id":"f","name":"D","trigger":{"type":"message"},"nodes":[{"id":"a","kind":"send","body":"x"}],"edges":[]}`
	invalido, _ := llamar(t, srv, "flow_validate", `{"flow": `+roto+`}`)
	if invalido["engineValid"] != false {
		t.Fatalf("un grafo que el motor rechaza debe salir con engineValid=false: %v", invalido)
	}
}

func TestFlowPutRechazaCambiarElTipoDeDisparo(t *testing.T) {
	srv, store := nuevoServidor()
	programado := strings.Replace(flujoValido, `"trigger": {"type": "message"}`,
		`"trigger": {"type": "schedule", "cron": "0 9 * * *", "viewId": "v1"}`, 1)
	payload, isError := llamar(t, srv, "flow_put",
		`{"botId":"bot-propio","flowId":"flujo-1","flow":`+programado+`,"expectedChecksum":"x"}`)
	if !isError || payload["code"] != "trigger_type_mismatch" {
		t.Fatalf("cambiar el tipo de disparo editando el grafo debería rechazarse: %v", payload)
	}
	if store.escrituras != 0 {
		t.Fatalf("se escribió pese al tipo de disparo distinto")
	}
}

func TestNingunaCapacidadCruzaLaOrganizacion(t *testing.T) {
	srv, _ := nuevoServidor()
	casos := []struct {
		tool string
		args string
	}{
		{"flow_get", `{"botId":"bot-ajeno"}`},
		{"flow_get", `{"botId":"bot-ajeno","flowId":"flujo-ajeno"}`},
		{"flow_validate", `{"botId":"bot-ajeno","flow":` + flujoValido + `}`},
		{"flow_put", `{"botId":"bot-ajeno","flowId":"flujo-ajeno","flow":` + flujoValido + `,"expectedChecksum":"x"}`},
	}
	for _, caso := range casos {
		payload, isError := llamar(t, srv, caso.tool, caso.args)
		if !isError || payload["code"] != "bot_not_found" {
			t.Fatalf("%s alcanzó un bot de otra organización: %v", caso.tool, payload)
		}
	}
	// El listado tampoco los enseña, aunque estén en la misma base.
	if ids := idsDeBots(t, srv); ids["bot-ajeno"] {
		t.Fatalf("el listado de bots enseñó uno de otra organización: %v", ids)
	} else if !ids["bot-propio"] || !ids["bot-hermano"] || len(ids) != 2 {
		t.Fatalf("el listado no trae los bots propios: %v", ids)
	}
}

func idsDeBots(t *testing.T, srv *session) map[string]bool {
	t.Helper()
	listado, isError := llamar(t, srv, "flow_get", `{}`)
	if isError {
		t.Fatalf("flow_get sin argumentos falló: %v", listado)
	}
	bots, _ := listado["bots"].([]any)
	ids := make(map[string]bool, len(bots))
	for _, bot := range bots {
		entrada, _ := bot.(map[string]any)
		id, _ := entrada["id"].(string)
		ids[id] = true
	}
	return ids
}

func idsDeFlujos(t *testing.T, srv *session, botID string) map[string]bool {
	t.Helper()
	listado, isError := llamar(t, srv, "flow_get", `{"botId":"`+botID+`"}`)
	if isError {
		t.Fatalf("flow_get de %s falló: %v", botID, listado)
	}
	flows, _ := listado["flows"].([]any)
	ids := make(map[string]bool, len(flows))
	for _, flow := range flows {
		entrada, _ := flow.(map[string]any)
		id, _ := entrada["id"].(string)
		ids[id] = true
	}
	return ids
}

// El alcance tiene que sostenerse en las cuatro capacidades. Aquí el vecino no
// es de otra organización —eso ya lo tapa el aislamiento por org— sino un flujo
// hermano dentro del mismo bot: solo el alcance puede esconderlo.
func TestKeyAcotadaNoAlcanzaOtroFlujoDeSuOrganizacion(t *testing.T) {
	srv, store := servidorAcotado()

	if ids := idsDeFlujos(t, srv, "bot-propio"); !ids["flujo-1"] || ids["flujo-2"] || len(ids) != 1 {
		t.Fatalf("el listado de flujos no respeta el alcance: %v", ids)
	}
	// Un bot entero fuera de alcance desaparece del listado en vez de aparecer
	// vacío: enseñarlo ya diría que existe.
	if ids := idsDeBots(t, srv); !ids["bot-propio"] || ids["bot-hermano"] || len(ids) != 1 {
		t.Fatalf("el listado de bots no respeta el alcance: %v", ids)
	}

	casos := []struct {
		nombre string
		tool   string
		args   string
		codigo string
	}{
		{"leer un flujo vecino", "flow_get", `{"botId":"bot-propio","flowId":"flujo-2"}`, "flow_not_found"},
		{"listar un bot fuera de alcance", "flow_get", `{"botId":"bot-hermano"}`, "bot_not_found"},
		{"leer un flujo de ese bot", "flow_get", `{"botId":"bot-hermano","flowId":"flujo-hermano"}`, "flow_not_found"},
		{"validar contra un bot fuera de alcance", "flow_validate",
			`{"botId":"bot-hermano","flow":` + flujoValido + `}`, "bot_not_found"},
		{"escribir un flujo vecino", "flow_put",
			`{"botId":"bot-propio","flowId":"flujo-2","flow":` + flujoValido + `,"expectedChecksum":"x"}`, "flow_not_found"},
		{"escribir en un bot fuera de alcance", "flow_put",
			`{"botId":"bot-hermano","flowId":"flujo-hermano","flow":` + flujoValido + `,"expectedChecksum":"x"}`, "flow_not_found"},
	}
	for _, caso := range casos {
		payload, isError := llamar(t, srv, caso.tool, caso.args)
		if !isError || payload["code"] != caso.codigo {
			t.Fatalf("%s: se esperaba %s y llegó %v", caso.nombre, caso.codigo, payload)
		}
	}
	if store.escrituras != 0 {
		t.Fatalf("un token acotado escribió fuera de su alcance")
	}

	// Dentro del alcance sigue funcionando todo, incluido escribir.
	lectura, isError := llamar(t, srv, "flow_get", `{"botId":"bot-propio","flowId":"flujo-1"}`)
	if isError {
		t.Fatalf("el token no puede leer su propio flujo: %v", lectura)
	}
	flow, _ := lectura["flow"].(map[string]any)
	checksum, _ := flow["draftChecksum"].(string)
	nuevo := strings.Replace(flujoValido, `"body": "hola"`, `"body": "acotado"`, 1)
	guardado, isError := llamar(t, srv, "flow_put",
		`{"botId":"bot-propio","flowId":"flujo-1","flow":`+nuevo+`,"expectedChecksum":"`+checksum+`"}`)
	if isError || guardado["saved"] != true {
		t.Fatalf("el token no puede escribir el flujo que sí alcanza: %v", guardado)
	}
	// flow_spec no apunta a ningún flujo, así que lo que le corresponde del
	// alcance es declararlo: sin esto el modelo descubre sus límites a base de
	// errores y concluye que los flujos fueron borrados.
	spec, _ := llamar(t, srv, "flow_spec", `{"section":"rules"}`)
	access, _ := spec["access"].(map[string]any)
	if access["scoped"] != true {
		t.Fatalf("flow_spec no declara que el token está acotado: %v", spec)
	}
}

// Una key sin lista de flujos alcanza toda su organización. Es el caso por
// defecto al crearla desde el panel sin marcar ninguno.
func TestKeySinAlcanceConservaElAccesoCompleto(t *testing.T) {
	srv, _ := nuevoServidor()
	if srv.key.Scoped() {
		t.Fatalf("la key del fixture no debería estar acotada")
	}
	if ids := idsDeFlujos(t, srv, "bot-propio"); !ids["flujo-1"] || !ids["flujo-2"] {
		t.Fatalf("un token sin alcance debe ver todos los flujos del bot: %v", ids)
	}
	if ids := idsDeBots(t, srv); !ids["bot-propio"] || !ids["bot-hermano"] {
		t.Fatalf("un token sin alcance debe ver todos los bots de su organización: %v", ids)
	}
	for _, args := range []string{
		`{"botId":"bot-propio","flowId":"flujo-2"}`,
		`{"botId":"bot-hermano","flowId":"flujo-hermano"}`,
	} {
		if payload, isError := llamar(t, srv, "flow_get", args); isError {
			t.Fatalf("un token sin alcance no pudo leer %s: %v", args, payload)
		}
	}
	if payload, isError := llamar(t, srv, "flow_validate",
		`{"botId":"bot-hermano","flow":`+flujoValido+`}`); isError {
		t.Fatalf("un token sin alcance no pudo validar contra un bot propio: %v", payload)
	}
	spec, _ := llamar(t, srv, "flow_spec", `{}`)
	access, _ := spec["access"].(map[string]any)
	if access["scoped"] != false {
		t.Fatalf("flow_spec declaró acotado un token completo: %v", spec)
	}
}

// La prueba que da sentido al transporte HTTP. Un mismo Service atiende a
// varias personas, así que si la key viviera fuera de la petición —en el
// Service, en una variable de paquete o en una caché— la segunda heredaría los
// límites o los permisos de la primera. Se intercalan a propósito: un fallo de
// estado compartido se manifiesta en la petición siguiente, no en la propia.
func TestDosKeysDistintasNoSeContaminanEnElMismoProceso(t *testing.T) {
	app, _ := appDePrueba()

	for vuelta := 0; vuelta < 3; vuelta++ {
		limitado, isError := llamarHTTP(t, app, keyAcotada, "flow_get", `{"botId":"bot-propio"}`)
		if isError {
			t.Fatalf("vuelta %d: la key acotada no pudo listar: %v", vuelta, limitado)
		}
		if ids := idsDeLista(limitado, "flows"); !ids["flujo-1"] || ids["flujo-2"] || len(ids) != 1 {
			t.Fatalf("vuelta %d: la key acotada vio de más: %v", vuelta, ids)
		}

		total, isError := llamarHTTP(t, app, keyCompleta, "flow_get", `{"botId":"bot-propio"}`)
		if isError {
			t.Fatalf("vuelta %d: la key completa no pudo listar: %v", vuelta, total)
		}
		if ids := idsDeLista(total, "flows"); !ids["flujo-1"] || !ids["flujo-2"] {
			t.Fatalf("vuelta %d: la key completa heredó el alcance de la otra: %v", vuelta, ids)
		}

		vecino, isError := llamarHTTP(t, app, keyAcotada, "flow_get", `{"botId":"bot-propio","flowId":"flujo-2"}`)
		if !isError || vecino["code"] != "flow_not_found" {
			t.Fatalf("vuelta %d: la key acotada alcanzó el vecino: %v", vuelta, vecino)
		}
	}

	conAviso := leerCuerpo(t, postMCP(t, app, keyAcotada, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	sinAviso := leerCuerpo(t, postMCP(t, app, keyCompleta, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if !strings.Contains(conAviso, "solo alcanza") {
		t.Fatalf("la key acotada no recibió el aviso de alcance: %s", conAviso)
	}
	if strings.Contains(sinAviso, "solo alcanza") {
		t.Fatalf("la key completa heredó el aviso de la acotada: %s", sinAviso)
	}
}

func idsDeLista(payload map[string]any, clave string) map[string]bool {
	entradas, _ := payload[clave].([]any)
	ids := make(map[string]bool, len(entradas))
	for _, entrada := range entradas {
		objeto, _ := entrada.(map[string]any)
		id, _ := objeto["id"].(string)
		ids[id] = true
	}
	return ids
}

// Revocar desde el panel tiene que notarse en la llamada siguiente, no cuando
// expire una caché. Es la razón de resolver la fila en cada petición y el motivo
// por el que se sustituyó la firma HMAC, que no se podía revocar sin invalidar
// las credenciales de todo el mundo.
func TestUnaKeyRevocadaDejaDeFuncionarDeInmediato(t *testing.T) {
	app, store := appDePrueba()
	cuerpo := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	if response := postMCP(t, app, keyCompleta, cuerpo); response.StatusCode != http.StatusOK {
		t.Fatalf("la key debería funcionar antes de revocarla: %d", response.StatusCode)
	}

	revocada := time.Now().Add(-time.Second)
	store.keys[keyCompleta].RevokedAt = &revocada

	response := postMCP(t, app, keyCompleta, cuerpo)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("la key revocada siguió entrando: %d", response.StatusCode)
	}
	// Y no se distingue de una que nunca existió: separar los dos casos sería
	// media pista sobre qué credenciales probar.
	revocadaTexto := leerCuerpo(t, response)
	inexistente := leerCuerpo(t, postMCP(t, app, models.MCPKeyPrefix+"nunca-existio", cuerpo))
	if revocadaTexto != inexistente {
		t.Fatalf("revocada e inexistente responden distinto:\n%s\n%s", revocadaTexto, inexistente)
	}

	// Y si el filtro de la consulta se cayera —un SELECT sin `revoked_at IS
	// NULL`—, el servicio sigue rechazándola. Sin esta comprobación propia, ese
	// fallo no lo vería ninguna prueba de este paquete.
	store.devolverRevocadas = true
	if response := postMCP(t, app, keyCompleta, cuerpo); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("el servicio confió en el filtro de la consulta: %d", response.StatusCode)
	}
	store.devolverRevocadas = false

	// La caducidad es la otra forma de que deje de valer, y se comprueba igual.
	store.keys[keyCompleta].RevokedAt = nil
	caducada := time.Now().Add(-time.Minute)
	store.keys[keyCompleta].ExpiresAt = &caducada
	if response := postMCP(t, app, keyCompleta, cuerpo); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("una key caducada siguió entrando: %d", response.StatusCode)
	}
}

func TestElTransporteExigeUnaKeyVivaEnCadaPeticion(t *testing.T) {
	app, _ := appDePrueba()
	cuerpo := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	casos := []struct {
		nombre string
		key    string
	}{
		{"sin cabecera", ""},
		{"cadena sin el prefijo del servicio", "no-es-una-key"},
		{"prefijo correcto pero inexistente", models.MCPKeyPrefix + "aaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	for _, caso := range casos {
		response := postMCP(t, app, caso.key, cuerpo)
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: se esperaba 401 y llegó %d", caso.nombre, response.StatusCode)
		}
		// Sin esta cabecera un cliente MCP da el servidor por caído en vez de
		// pedir credenciales.
		if response.Header.Get("WWW-Authenticate") == "" {
			t.Fatalf("%s: falta WWW-Authenticate", caso.nombre)
		}
	}
}

// El alcance sale solo de la fila. Estos son los sitios por los que una petición
// podría intentar ampliarlo.
func TestElAlcanceNoSeAmpliaDesdeLaPeticion(t *testing.T) {
	app, _ := appDePrueba()

	payload, isError := llamarHTTP(t, app, keyAcotada, "flow_get",
		`{"botId":"bot-hermano","orgId":"`+orgPropia+`","flowIds":["flujo-2"],"scope":"all"}`)
	if !isError || payload["code"] != "bot_not_found" {
		t.Fatalf("el cuerpo amplió el alcance: %v", payload)
	}

	request := httptest.NewRequest(http.MethodPost,
		"/mcp/flows?orgId="+orgPropia+"&flowIds=flujo-2",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"flow_get","arguments":{"botId":"bot-propio"}}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+keyAcotada)
	request.Header.Set("X-Org-Id", orgPropia)
	request.Header.Set("X-Mcp-Flows", "flujo-2")
	response, err := app.Test(request, -1)
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if cuerpo := leerCuerpo(t, response); strings.Contains(cuerpo, "flujo-2") {
		t.Fatalf("una cabecera o la query ampliaron el alcance: %s", cuerpo)
	}
}

// Sin base no se puede resolver ninguna key, así que el endpoint responde 503 en
// vez de abrirse. La ruta sigue existiendo: una que desaparece manda a buscar el
// fallo al sitio equivocado.
func TestSinBaseElServicioRespondeNoConfigurado(t *testing.T) {
	app := fiber.New()
	NewService(nil, nil).Register(app)
	response := postMCP(t, app, keyCompleta, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("se esperaba 503 y llegó %d", response.StatusCode)
	}
}

// El canal servidor→cliente y el borrado de sesión no se ofrecen. La spec pide
// decirlo con un 405, no dejar la conexión colgando.
func TestElServicioDeclaraQueNoOfreceCanalDeEventos(t *testing.T) {
	app, _ := appDePrueba()
	for _, metodo := range []string{http.MethodGet, http.MethodDelete} {
		request := httptest.NewRequest(metodo, "/mcp/flows", nil)
		request.Header.Set("Authorization", "Bearer "+keyCompleta)
		response, err := app.Test(request, -1)
		if err != nil {
			t.Fatalf("%s: %v", metodo, err)
		}
		if response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s debería responder 405 y respondió %d", metodo, response.StatusCode)
		}
	}
}

// El uso se anota, pero anotarlo no puede condicionar el acceso ni convertir
// cada lectura en una escritura cara: models.TouchMCPKey filtra por intervalo en
// el propio UPDATE.
func TestElUsoSeAnotaSinBloquearLaRespuesta(t *testing.T) {
	app, store := appDePrueba()
	for i := 0; i < 3; i++ {
		if response := postMCP(t, app, keyCompleta,
			`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); response.StatusCode != http.StatusOK {
			t.Fatalf("petición %d respondió %d", i, response.StatusCode)
		}
	}
	if store.tocadas != 3 {
		t.Fatalf("se esperaba una anotación por petición autorizada y hubo %d", store.tocadas)
	}
}

func TestMetodoDesconocidoNoTumbaLaSesion(t *testing.T) {
	app, _ := appDePrueba()
	desconocido := postMCP(t, app, keyCompleta, `{"jsonrpc":"2.0","id":1,"method":"inventado"}`)
	// Un método que no existe es un error de JSON-RPC dentro de un 200, no un
	// error HTTP: el transporte funcionó, lo que falló fue el mensaje.
	if desconocido.StatusCode != http.StatusOK {
		t.Fatalf("un método desconocido no es un fallo de transporte: %d", desconocido.StatusCode)
	}
	if cuerpo := leerCuerpo(t, desconocido); !strings.Contains(cuerpo, "-32601") {
		t.Fatalf("se esperaba -32601 y llegó: %s", cuerpo)
	}
	siguiente := leerCuerpo(t, postMCP(t, app, keyCompleta, `{"jsonrpc":"2.0","id":2,"method":"ping"}`))
	if !strings.Contains(siguiente, `"result"`) {
		t.Fatalf("el servicio dejó de atender tras un método desconocido: %s", siguiente)
	}
}
