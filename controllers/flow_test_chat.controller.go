package controllers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/engine"
	"github.com/Yzx7/sacs-chatbots/engine/ai"
	"github.com/Yzx7/sacs-chatbots/engine/tools"
	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
)

// Chat de prueba del borrador.
//
// Ejecuta el grafo que el autor tiene abierto en el editor —aunque no esté
// guardado y desde luego sin publicar— contra un cliente simulado. El endpoint
// es sin estado: no crea chats, contactos, `flow_runs` ni mensajes, y el estado
// de la conversación viaja de ida y vuelta en el cuerpo.
//
// Las dos asimetrías que definen la pieza:
//
//   - **El agente se ejecuta de verdad y cuesta créditos.** Probar el prompt y
//     las ramas es el motivo de existir del chat de prueba; un agente simulado
//     no probaría nada.
//   - **Las herramientas se simulan.** Sus ejecutores escriben en Data, cierran
//     pedidos y mueven dinero y créditos. Pero la simulación **no** devuelve un
//     error: eso mandaría al flujo por la rama `error` y el autor acabaría
//     probando el recorrido equivocado. Devuelve un resultado sintético con la
//     misma forma que el real, para que el grafo continúe por `ok`.

const (
	// El cuerpo transporta el grafo entero del editor más el estado del turno
	// anterior. 1 MiB deja sitio a un flujo grande y sigue cortando un envío
	// disparatado antes de decodificarlo.
	maxFlowTestBodyBytes = 1 << 20
	// A partir de aquí se avisa de que el estado está creciendo. No se recorta:
	// quitarle variables al estado cambiaría el recorrido y el autor estaría
	// probando otro flujo. El aviso llega antes de que el reenvío choque contra
	// maxFlowTestBodyBytes y deje la conversación sin salida.
	maxFlowTestStateBytes = 256 << 10
	// Espeja maxMessageRunes del motor: un mensaje más largo se truncaría al
	// entrar en el historial, así que aceptarlo sería prometer lo que no se hace.
	maxFlowTestMessageRunes = 4000
)

// Códigos de error del chat de prueba. Viajan en `data.code` dentro del envelope
// estándar, como ya hace el conflicto del Copilot: el panel necesita distinguir
// «recarga créditos» de «tu borrador no valida» sin leer el texto en castellano.
const (
	flowTestErrBody             = "invalid_body"
	flowTestErrBodyTooLarge     = "body_too_large"
	flowTestErrInputType        = "invalid_input_type"
	flowTestErrDraft            = "invalid_draft"
	flowTestErrState            = "invalid_state"
	flowTestErrCredits          = "insufficient_credits"
	flowTestErrAgentUnavailable = "agent_unavailable"
)

// Códigos de aviso. `scope` separa lo estructural —se repetiría idéntico en cada
// turno, así que el panel lo enseña una vez— de lo que pasó en este turno
// concreto.
const (
	flowTestWarnNoContact       = "no_contact_context"
	flowTestWarnToolsSimulated  = "tools_simulated"
	flowTestWarnEmptyReads      = "empty_reads"
	flowTestWarnEngineStopped   = "engine_stopped"
	flowTestWarnNoSends         = "no_sends"
	flowTestWarnTemplateSkipped = "template_send_skipped"
	flowTestWarnSendMarkers     = "unresolved_markers_in_send"
	flowTestWarnPromptMarkers   = "unresolved_markers_in_instruction"
	flowTestWarnUsageNotStored  = "usage_not_recorded"
	flowTestWarnStateTooLarge   = "state_too_large"
)

const (
	flowTestScopeSession = "session"
	flowTestScopeTurn    = "turn"
)

// errFlowTestNoCredits distingue el agotamiento de saldo de cualquier otro fallo
// del bloque. El motor devuelve tal cual el error del adaptador, así que
// sobrevive a `engine.Advance` y el handler puede responder 402 con su código en
// vez de un aviso genérico dentro de un 200.
var errFlowTestNoCredits = errors.New("saldo de créditos agotado: recarga para probar bloques de IA")

// flowTestMarkerRe espeja varRe del motor. Sirve para avisar de los `{marcador}`
// que quedaron literales por falta de la variable, que es exactamente lo que le
// llegaría al modelo o al cliente.
var flowTestMarkerRe = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_.]*)\}`)

type flowTestWarning struct {
	Code    string `json:"code"`
	Scope   string `json:"scope"`
	Message string `json:"message"`
}

// flowTestToolCall es lo que el panel enseña al autor por cada herramienta que
// no se ejecutó.
//
// `nodeId` solo viaja cuando la llamada la hizo el modelo desde un bloque
// `agent`: ahí el nodo se conoce por `AgentRequest.NodeID`. Cuando la llamada
// viene de un bloque `tool` del grafo va vacío, porque `Deps.Tool` recibe
// `(ref, args, vars)` y el motor no le pasa el nodo. Deducirlo casando los
// argumentos contra el grafo sería adivinar: dos bloques con el mismo `toolRef`
// y los mismos argumentos son perfectamente legales.
type flowTestToolCall struct {
	Ref       string            `json:"ref"`
	Source    string            `json:"source"` // graph | agent
	NodeID    string            `json:"nodeId,omitempty"`
	Simulated bool              `json:"simulated"`
	Args      map[string]string `json:"args"`
	Result    string            `json:"result"`
	Note      string            `json:"note"`
}

type flowTestTurnResponse struct {
	Sends     []string           `json:"sends"`
	State     json.RawMessage    `json:"state"`
	Handoff   bool               `json:"handoff"`
	Finished  bool               `json:"finished"`
	ToolCalls []flowTestToolCall `json:"toolCalls"`
	CostUSD   float64            `json:"costUsd"`
	Warnings  []flowTestWarning  `json:"warnings"`
}

// flowTestRun acumula lo observable del turno.
//
// Lleva mutex porque el bucle agéntico podría pasar a ejecutar en paralelo las
// llamadas de un mismo paso —hoy son secuenciales— y una carrera ahí no daría
// un fallo visible, sino un `toolCalls` al que le faltan entradas.
type flowTestRun struct {
	mu       sync.Mutex
	calls    []flowTestToolCall
	warnings []flowTestWarning
	costUSD  float64
	// writes cuenta las escrituras simuladas para dar identificadores estables y
	// distintos a cada registro fingido dentro del mismo turno.
	writes int
	// emptyReads recuerda que alguna lectura simulada devolvió cero resultados,
	// porque eso cambia el recorrido que el autor acaba probando.
	emptyReads bool
}

func (r *flowTestRun) warn(code, scope, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.warnings {
		if existing.Code == code && existing.Message == message {
			return
		}
	}
	r.warnings = append(r.warnings, flowTestWarning{Code: code, Scope: scope, Message: message})
}

func (r *flowTestRun) addCost(usd float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.costUSD += usd
}

func (r *flowTestRun) record(call flowTestToolCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
}

func (r *flowTestRun) nextWriteSequence() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes++
	return r.writes
}

func (r *flowTestRun) markEmptyRead() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.emptyReads = true
}

// failFlowTest responde con el envelope estándar y un código estable en `data`.
// Sigue el precedente del conflicto del Copilot: el mensaje es para la persona,
// el código para el panel.
func (con *Controller) failFlowTest(c *fiber.Ctx, status int, code, message string, extra map[string]any) error {
	data := map[string]any{"code": code}
	for key, value := range extra {
		data[key] = value
	}
	return c.Status(status).JSON(types.ErrData(message, data))
}

// POST /bots/:botId/flows/:flowId/test/turns
func (con *Controller) CreateBotFlowTestTurn(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	if len(c.Body()) > maxFlowTestBodyBytes {
		return con.failFlowTest(c, fiber.StatusRequestEntityTooLarge, flowTestErrBodyTooLarge,
			fmt.Sprintf("el cuerpo supera %d bytes", maxFlowTestBodyBytes), nil)
	}
	var body struct {
		WorkingDraft json.RawMessage `json:"workingDraft"`
		State        json.RawMessage `json:"state"`
		Message      string          `json:"message"`
		InputType    string          `json:"inputType"`
	}
	if err := decodeStrictJSONBody(c.Body(), &body); err != nil {
		return con.failFlowTest(c, fiber.StatusBadRequest, flowTestErrBody,
			"input inválido: "+err.Error(), nil)
	}
	if len([]rune(body.Message)) > maxFlowTestMessageRunes {
		return con.failFlowTest(c, fiber.StatusBadRequest, flowTestErrBody,
			fmt.Sprintf("message excede %d caracteres", maxFlowTestMessageRunes), nil)
	}
	// El chat de prueba solo habla texto. Mientras `engine.Result.Sends` sea
	// []string no existe emisor de formatos de salida (CLAUDE.md §3), y de
	// entrada tampoco hay bytes que adjuntar: aceptar «image» dejaría al autor
	// creyendo que probó su rama de comprobantes cuando el bloque visual nunca
	// recibió una imagen. La restricción vive aquí y en ningún otro sitio.
	if inputType := strings.TrimSpace(body.InputType); inputType != "" && inputType != "text" {
		return con.failFlowTest(c, fiber.StatusBadRequest, flowTestErrInputType,
			"el chat de prueba solo admite inputType \"text\": todavía no puede adjuntar archivos ni emitir formatos de salida", nil)
	}
	if len(body.WorkingDraft) == 0 {
		return con.failFlowTest(c, fiber.StatusBadRequest, flowTestErrDraft,
			"workingDraft es obligatorio", nil)
	}
	// Un grafo inválido no se ejecuta: el motor fallaría más adentro y con un
	// síntoma que no señala el bloque mal configurado.
	if problem := validateFlowDefinition(body.WorkingDraft, flow.TriggerType); problem != "" {
		return con.failFlowTest(c, fiber.StatusBadRequest, flowTestErrDraft,
			"el borrador no es válido: "+problem, map[string]any{"problem": problem})
	}
	var graph engine.Flow
	if err := json.Unmarshal(body.WorkingDraft, &graph); err != nil {
		return con.failFlowTest(c, fiber.StatusBadRequest, flowTestErrDraft,
			"workingDraft no es JSON del editor", nil)
	}
	state, problem := parseFlowTestState(body.State, flow.ID)
	if problem != "" {
		return con.failFlowTest(c, fiber.StatusBadRequest, flowTestErrState, problem, nil)
	}

	agentic := flowTestUsesAgents(&graph)
	if agentic && con.Env.TextAgent == nil && con.Env.OrchestratorAgent == nil && con.Env.VisionAgent == nil {
		return con.failFlowTest(c, fiber.StatusServiceUnavailable, flowTestErrAgentUnavailable,
			"el flujo tiene bloques de IA y este backend no tiene ningún proveedor configurado", nil)
	}
	if agentic {
		// Se comprueba antes de empezar: un turno que se queda a medias por saldo
		// deja al autor viendo un recorrido truncado en vez de un motivo. Solo se
		// exige si el grafo va a usar IA, para que un flujo sin agentes se pueda
		// probar con el monedero a cero.
		wallet, walletErr := models.GetOrCreateCreditWallet(c.Context(), con.Env.Postgres, bot.OrgID)
		if walletErr != nil {
			return con.failFlow(c, "GetOrCreateCreditWallet", bot.ID, walletErr,
				"no se pudo comprobar el saldo de créditos")
		}
		if wallet.Balance <= 0 {
			return con.failFlowTest(c, fiber.StatusPaymentRequired, flowTestErrCredits,
				"saldo de créditos agotado; recarga antes de probar un flujo con IA", nil)
		}
	}

	turnID := flowTestTurnID()
	run := &flowTestRun{warnings: flowTestSessionWarnings(&graph)}

	deps := engine.Deps{
		// Sin contacto a propósito: el chat de prueba no personifica a nadie. Las
		// consecuencias se avisan en `warnings`, no se disimulan con datos falsos.
		Context:   map[string]string{},
		InputType: "text",
		WaID:      "flow-test:" + turnID,
		Input: engine.InboundInput{
			ID:          "flow-test:" + turnID,
			EventType:   "message",
			ContentType: "text",
			Text:        body.Message,
		},
	}
	if agentic {
		deps.AgentStructured = con.flowTestAgent(c.Context(), bot, flow.ID, turnID, run)
	}
	// Ni un solo camino llega a un ejecutor real: los que escriben en Data,
	// cierran pedidos o acreditan créditos no tienen por qué existir aquí.
	deps.Tool = func(ref string, args, _ map[string]string) (string, error) {
		return run.simulateGraphTool(ref, args)
	}

	result, advanceErr := engine.Advance(&graph, state, body.Message, deps)
	if errors.Is(advanceErr, errFlowTestNoCredits) {
		// El saldo se agotó durante el turno (otra sesión lo consumió). Es un
		// motivo distinto de «tu flujo falló» y merece su propio código.
		return con.failFlowTest(c, fiber.StatusPaymentRequired, flowTestErrCredits,
			advanceErr.Error(), nil)
	}
	if advanceErr != nil {
		// El motor devuelve el estado reanudable junto al error; se conserva para
		// que el autor pueda reintentar el mismo bloque tras corregir la causa.
		run.warn(flowTestWarnEngineStopped, flowTestScopeTurn,
			"El recorrido se detuvo en un bloque: "+advanceErr.Error())
	}
	for index, message := range result.Sends {
		if markers := flowTestUnresolvedMarkers(message); len(markers) > 0 {
			run.warn(flowTestWarnSendMarkers, flowTestScopeTurn, fmt.Sprintf(
				"El mensaje %d saldría con variables sin resolver: %s", index+1, strings.Join(markers, ", ")))
		}
	}
	if len(result.Templates) > 0 {
		run.warn(flowTestWarnTemplateSkipped, flowTestScopeTurn,
			"El flujo intentó enviar una plantilla aprobada; el chat de prueba no envía nada por WhatsApp.")
	}
	if len(result.Sends) == 0 && advanceErr == nil {
		run.warn(flowTestWarnNoSends, flowTestScopeTurn,
			"El turno terminó sin ningún mensaje para el cliente.")
	}
	if run.emptyReads {
		run.warn(flowTestWarnEmptyReads, flowTestScopeTurn,
			"Las lecturas simuladas devolvieron cero resultados (found=false): lo que se probó es el recorrido de «no hay datos».")
	}

	response := flowTestTurnResponse{
		Sends:     result.Sends,
		State:     json.RawMessage("null"),
		Handoff:   result.Handoff,
		Finished:  result.Done,
		ToolCalls: run.calls,
		CostUSD:   run.costUSD,
		Warnings:  run.warnings,
	}
	if response.Sends == nil {
		response.Sends = []string{}
	}
	if response.ToolCalls == nil {
		response.ToolCalls = []flowTestToolCall{}
	}
	if response.Warnings == nil {
		response.Warnings = []flowTestWarning{}
	}
	if result.State != nil {
		// Se sella con el flujo igual que hace el webhook: un estado pegado desde
		// otro grafo reanudaría en un nodeId que aquí significa otra cosa.
		result.State.FlowID = flow.ID
		if raw, err := json.Marshal(result.State); err == nil {
			response.State = raw
			if len(raw) > maxFlowTestStateBytes {
				run.warn(flowTestWarnStateTooLarge, flowTestScopeTurn, fmt.Sprintf(
					"El estado de la conversación ocupa %d KiB; al superar %d KiB el siguiente turno será rechazado. Empieza una conversación nueva.",
					len(raw)>>10, maxFlowTestBodyBytes>>10))
				response.Warnings = run.warnings
			}
		}
	}
	return con.ok(c, "ok", response)
}

// flowTestUsesAgents dice si el grafo va a necesitar el proveedor de IA. De ahí
// depende exigir saldo: un flujo sin agentes debe poder probarse con el
// monedero a cero, porque no va a gastar nada.
func flowTestUsesAgents(flow *engine.Flow) bool {
	for index := range flow.Nodes {
		if flow.Nodes[index].Kind == "agent" {
			return true
		}
	}
	return false
}

// parseFlowTestState acepta el estado que devolvió el turno anterior. `null`,
// ausente y `{}` significan empezar de cero.
func parseFlowTestState(raw json.RawMessage, flowID string) (*engine.State, string) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return nil, ""
	}
	var state engine.State
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, "state inválido: no es el objeto devuelto por el turno anterior"
	}
	if state.NodeID == "" {
		return nil, "state inválido: falta nodeId"
	}
	if state.FlowID != "" && state.FlowID != flowID {
		return nil, "state pertenece a otro flujo: sus nodos y variables no significan lo mismo aquí"
	}
	return &state, ""
}

// flowTestSessionWarnings avisa de lo que el chat de prueba no puede reproducir
// y que se repetiría idéntico en cada turno, así que el panel lo enseña una vez.
// El caso principal es el contexto de contacto: sin él, un flujo como Sistemuino
// se va siempre por la rama «sin perfil» y el autor podría concluir que su
// Router está mal.
func flowTestSessionWarnings(flow *engine.Flow) []flowTestWarning {
	var warnings []flowTestWarning
	linksContact := false
	usesContactVars := false
	simulatesTools := false
	for index := range flow.Nodes {
		node := &flow.Nodes[index]
		if node.Kind == "tool" || len(node.Tools) > 0 {
			simulatesTools = true
		}
		if strings.EqualFold(strings.TrimSpace(node.Args["linkCurrentContact"]), "true") {
			linksContact = true
		}
		for _, text := range flowTestNodeTexts(node) {
			for _, marker := range flowTestMarkerRe.FindAllStringSubmatch(text, -1) {
				if strings.HasPrefix(marker[1], "contact_") || strings.HasPrefix(marker[1], "contact.") {
					usesContactVars = true
				}
			}
		}
	}
	if linksContact || usesContactVars {
		warnings = append(warnings, flowTestWarning{
			Code: flowTestWarnNoContact, Scope: flowTestScopeSession,
			Message: "El chat de prueba no personifica a ningún contacto: las variables de contacto quedan vacías y los bloques vinculados al contacto actual no encuentran su perfil.",
		})
	}
	if simulatesTools {
		warnings = append(warnings, flowTestWarning{
			Code: flowTestWarnToolsSimulated, Scope: flowTestScopeSession,
			Message: "Las herramientas de este flujo no se ejecutan: no se escribe en Data, no se crean pedidos y no se mueven créditos. Cada llamada devuelve un resultado sintético con la forma real.",
		})
	}
	return warnings
}

func flowTestNodeTexts(node *engine.Node) []string {
	texts := []string{node.Body, node.Instruction, node.Expression}
	texts = append(texts, node.TemplateParams...)
	for _, value := range node.Args {
		texts = append(texts, value)
	}
	for _, value := range node.Params {
		texts = append(texts, value)
	}
	for _, routerCase := range node.Cases {
		texts = append(texts, routerCase.Expression)
	}
	return texts
}

func flowTestUnresolvedMarkers(text string) []string {
	matches := flowTestMarkerRe.FindAllString(text, -1)
	seen := make(map[string]struct{}, len(matches))
	unique := make([]string, 0, len(matches))
	for _, marker := range matches {
		if _, repeated := seen[marker]; repeated {
			continue
		}
		seen[marker] = struct{}{}
		unique = append(unique, marker)
	}
	return unique
}

// flowTestTurnID identifica el turno. No hay fila que lo guarde: existe para
// que la clave de idempotencia del cobro de créditos sea única cuando el
// proveedor no devuelve un request id propio.
func flowTestTurnID() string {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		// Sin aleatoriedad dos turnos podrían compartir clave y el segundo no se
		// cobraría; el reloj en nanosegundos basta para separarlos.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buffer)
}

// flowTestAgent ejecuta el agente de verdad, con el mismo camino que el webhook:
// mismos proveedores, mismo reintento y mismo cobro de créditos. Lo único que
// cambia es que sus herramientas quedan simuladas y que el consumo se marca como
// proveniente de la prueba.
func (con *Controller) flowTestAgent(ctx context.Context, bot *models.Bot, flowID, turnID string,
	run *flowTestRun) func(engine.AgentRequest) (engine.AgentResult, error) {
	return func(request engine.AgentRequest) (engine.AgentResult, error) {
		if containsString(request.Accepts, "image") {
			// El agente visual solo existe con bytes: llamarlo sin imagen gastaría
			// créditos para obtener una lectura de la nada.
			return engine.AgentResult{}, fmt.Errorf(
				"el bloque %q necesita la imagen del mensaje y el chat de prueba no envía archivos", request.NodeID)
		}
		if markers := flowTestUnresolvedMarkers(request.Instruction); len(markers) > 0 {
			run.warn(flowTestWarnPromptMarkers, flowTestScopeTurn, fmt.Sprintf(
				"La instrucción de %s llega al modelo con variables sin resolver: %s",
				request.NodeID, strings.Join(markers, ", ")))
		}
		agentTools, toolExec, toolErr := flowTestAgentTooling(request.NodeID, request.Tools, run)
		if toolErr != nil {
			return engine.AgentResult{}, toolErr
		}
		wallet, walletErr := models.GetOrCreateCreditWallet(ctx, con.Env.Postgres, bot.OrgID)
		if walletErr != nil {
			return engine.AgentResult{}, fmt.Errorf("comprobar saldo de créditos: %w", walletErr)
		}
		if wallet.Balance <= 0 {
			// A diferencia del webhook no se responde el mensaje de reserva: aquí no
			// hay cliente al que atender, y fingir una rama de fallback le enseñaría
			// al autor un recorrido que su saldo, no su grafo, provocó.
			return engine.AgentResult{}, errFlowTestNoCredits
		}

		for attempt := 1; attempt <= 2; attempt++ {
			startedAt := time.Now()
			agentResult, usage, runErr := con.runAgent(ctx, bot.OrgID, request, agentTools, toolExec)
			duration := time.Since(startedAt)
			errorCode := ai.OutputErrorCode(runErr)
			if usage.Provider != "" {
				con.recordFlowTestUsage(ctx, bot, flowID, turnID, request.NodeID, attempt,
					duration, errorCode, agentResult.Branch, usage, runErr, run)
			}
			// Mismo reintento que el webhook: sin él, una salida mal formada que
			// producción sí recupera parecería un fallo del flujo del autor.
			if runErr == nil || attempt == 2 || !retryableAgentOutput(errorCode) {
				return agentResult, runErr
			}
		}
		return engine.AgentResult{}, fmt.Errorf("agente IA agotó sus intentos")
	}
}

// recordFlowTestUsage persiste el consumo y cobra los créditos.
//
// `purpose` sigue siendo flow_runtime: la base lo restringe por CHECK a
// flow_runtime/flow_authoring (022_flow_copilot.sql), y añadir un valor exigiría
// migrar una tabla ya en producción para una distinción que `metadata.source`
// hace igual de bien. Es lo que separa el gasto de probar del de atender.
func (con *Controller) recordFlowTestUsage(ctx context.Context, bot *models.Bot, flowID, turnID, nodeID string,
	attempt int, duration time.Duration, errorCode, branch string, usage ai.Usage, runErr error, run *flowTestRun) {
	outcome := "ok"
	if runErr != nil {
		outcome = "invalid_output"
	}
	metadataFields := map[string]any{
		"source":      "flow_test",
		"flow_id":     flowID,
		"turn_id":     turnID,
		"node_id":     nodeID,
		"attempt":     fmt.Sprint(attempt),
		"duration_ms": fmt.Sprint(duration.Milliseconds()),
	}
	if errorCode != "" {
		metadataFields["error_code"] = errorCode
	}
	if branch != "" {
		metadataFields["branch"] = branch
	}
	if usage.Steps > 1 {
		metadataFields["steps"] = fmt.Sprint(usage.Steps)
	}
	if len(usage.ToolTrace) > 0 {
		metadataFields["tool_trace"] = usage.ToolTrace
	}
	metadata, _ := json.Marshal(metadataFields)

	// La identidad económica es la petición del proveedor, igual que en el
	// webhook: una reentrega de la misma respuesta no puede cobrarse dos veces.
	creditKey := fmt.Sprintf("ai:%s:%s", usage.Provider, usage.RequestID)
	if strings.TrimSpace(usage.RequestID) == "" {
		creditKey = fmt.Sprintf("ai-flow-test:%s:%s:%d", turnID, nodeID, attempt)
	}
	event := models.AIUsageEventInput{
		OrganizationID: bot.OrgID, BotID: bot.ID,
		Purpose:  "flow_runtime",
		Provider: usage.Provider, Model: usage.Model, ProviderRequestID: usage.RequestID,
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		InputUSDPerMillion:       usage.Rates.InputPerMillion,
		OutputUSDPerMillion:      usage.Rates.OutputPerMillion,
		CacheReadUSDPerMillion:   usage.Rates.CacheReadPerMillion,
		CacheWriteUSDPerMillion:  usage.Rates.CacheWritePerMillion,
		Outcome:                  outcome, Metadata: metadata,
	}
	run.addCost(models.AIUsageCostUSD(event))
	if err := models.RecordAIUsageAndChargeCredits(ctx, con.Env.Postgres, models.AIUsageChargeInput{
		CreditType:     models.CreditTxAIRuntimeUsage,
		IdempotencyKey: creditKey,
		Notes:          fmt.Sprintf("Chat de prueba bot=%s flujo=%s nodo=%s", bot.ID, flowID, nodeID),
		Usage:          event,
	}); err != nil {
		con.Env.Logger.Error("flow test: registrar usage y créditos", "turn", turnID, "err", err.Error())
		run.warn(flowTestWarnUsageNotStored, flowTestScopeTurn,
			"No se pudo registrar el consumo de IA de este turno.")
	}
}

// flowTestAgentTooling espeja agentTooling: mismas fichas para el modelo —el
// prompt tiene que ser el de producción— y un ejecutor que no toca nada. El
// nodeID se captura aquí porque es el único punto del chat de prueba donde se
// sabe qué bloque está llamando.
func flowTestAgentTooling(nodeID string, nodeTools []engine.NodeTool, run *flowTestRun) ([]ai.AgentTool, ai.ToolExecutor, error) {
	if len(nodeTools) == 0 {
		return nil, nil, nil
	}
	specs := make([]ai.AgentTool, 0, len(nodeTools))
	config := make(map[string]map[string]string, len(nodeTools))
	for _, nodeTool := range nodeTools {
		spec := tools.Get(nodeTool.Ref)
		if spec == nil || !spec.ForAgent {
			return nil, nil, fmt.Errorf("herramienta %q no disponible para agentes", nodeTool.Ref)
		}
		specs = append(specs, ai.AgentTool{
			Name:        spec.Name,
			Description: spec.Description,
			InputSchema: spec.InputSchema,
		})
		config[nodeTool.Ref] = nodeTool.Config
	}
	exec := func(_ context.Context, name string, input json.RawMessage) (string, error) {
		return run.simulateAgentTool(nodeID, name, config[name], input)
	}
	return specs, exec, nil
}

// simulateGraphTool responde lo que habría respondido el ejecutor real, con su
// misma struct de salida. Reutilizar el tipo y no un mapa a mano es lo que
// impide que la forma sintética se separe de la verdadera sin que nadie lo note.
func (r *flowTestRun) simulateGraphTool(ref string, args map[string]string) (string, error) {
	result, note, err := flowTestGraphToolResult(ref, args, r)
	if err != nil {
		return "", err
	}
	r.record(flowTestToolCall{
		Ref: ref, Source: "graph", Simulated: true, Args: args, Result: result, Note: note,
	})
	return result, nil
}

func (r *flowTestRun) simulateAgentTool(nodeID, ref string, config map[string]string, input json.RawMessage) (string, error) {
	result, note := flowTestAgentToolResult(ref, config)
	r.record(flowTestToolCall{
		Ref: ref, Source: "agent", NodeID: nodeID, Simulated: true,
		Args: flowTestArgsFromJSON(input), Result: result, Note: note,
	})
	if flowTestReadRefs[ref] {
		r.markEmptyRead()
	}
	return result, nil
}

// flowTestReadRefs son las herramientas de solo lectura. Su simulación devuelve
// «no hay nada», que es un resultado válido y no un fallo.
var flowTestReadRefs = map[string]bool{
	"data_query": true, "catalog_search": true, "catalog_product": true, "dataset_query": true,
}

// flowTestSyntheticRecordID compone un identificador con forma de UUID pero
// imposible de confundir con uno real: los reales los genera PostgreSQL y no
// empiezan por doce ceros. Un id inventado con pinta legítima acabaría copiado a
// una consulta que no encontraría nada.
func flowTestSyntheticRecordID(sequence int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", sequence)
}

// flowTestGraphToolResult es el corazón de la decisión de diseño: cada rama
// devuelve la struct del ejecutor real, poblada con lo que el bloque pidió, para
// que el grafo salga por `ok`. Solo una herramienta desconocida es un error, y
// eso ya lo rechaza engine.Validate antes de llegar aquí.
func flowTestGraphToolResult(ref string, args map[string]string, run *flowTestRun) (string, string, error) {
	switch ref {
	case "data_mutate":
		operation := defaultString(args["operation"], "create")
		object := strings.TrimSpace(args["object"])
		values := map[string]string{}
		for key, value := range args {
			if !strings.HasPrefix(key, "field.") {
				continue
			}
			if field := strings.TrimSpace(strings.TrimPrefix(key, "field.")); field != "" {
				values[field] = value
			}
		}
		data, err := json.Marshal(values)
		if err != nil {
			return "", "", err
		}
		result := models.DataMutationResult{
			RecordID:  flowTestSyntheticRecordID(run.nextWriteSequence()),
			ObjectKey: object,
			Operation: operation,
			// `update` no crea; create y upsert sí en el caso normal, que es el que
			// el autor quiere ver funcionar.
			Created: operation != "update",
			Data:    data,
		}
		return flowTestJSON(result, fmt.Sprintf(
			"No se escribió nada. En producción esta operación %s habría guardado un registro real en «%s» con los campos mostrados.",
			operation, object))

	case "data_query":
		// Cero coincidencias es `ok` con found=false (§10), así que el grafo
		// continúa y el Router puede distinguirlo de un fallo. Se elige vacío y no
		// un registro inventado porque un dato falso terminaría dentro del prompt
		// del agente o en un mensaje al cliente, y el autor evaluaría su flujo
		// contra información que no existe.
		run.markEmptyRead()
		return flowTestJSON(models.DataQueryResult{Records: []models.DataQueryRecord{}}, fmt.Sprintf(
			"No se leyó la tabla «%s»: la lectura simulada devuelve found=false, que es un resultado válido.",
			strings.TrimSpace(args["object"])))

	case "catalog_search":
		run.markEmptyRead()
		return flowTestJSON(catalogResult{Records: []catalogRecord{}},
			"No se consultó la tienda conectada: la búsqueda simulada devuelve found=false.")

	case "catalog_product":
		run.markEmptyRead()
		return flowTestJSON(catalogResult{Records: []catalogRecord{}},
			"No se consultó la tienda conectada: el detalle simulado devuelve found=false.")

	case "dataset_query":
		run.markEmptyRead()
		return flowTestJSON(datasetResult{Records: []datasetRecord{}},
			"No se llamó al dataset externo: la lectura simulada devuelve found=false.")

	case "order_create":
		sequence := run.nextWriteSequence()
		return flowTestJSON(orderResult{
			OrderID:     int64(sequence),
			OrderNumber: fmt.Sprintf("PRUEBA-%d", sequence),
			Status:      "pending",
			Currency:    strings.TrimSpace(args["currency"]),
			ItemCount:   flowTestOrderLineCount(args),
			Summary:     "Pedido simulado por el chat de prueba: no se envió a la tienda.",
		}, "No se creó ningún pedido en la tienda conectada; los importes vienen a cero porque nadie los calculó.")

	case "payment_intent_create":
		sequence := run.nextWriteSequence()
		return flowTestJSON(paymentIntentResult{
			PaymentID: int64(sequence),
			OrderID:   flowTestInt64(args["orderId"]),
			Status:    "pending",
			Provider:  strings.TrimSpace(args["provider"]),
			Message:   "Cobro simulado por el chat de prueba: aquí irían las instrucciones de pago reales de la tienda.",
		}, "No se abrió ningún cobro en la tienda conectada.")

	case "payment_submit":
		return flowTestJSON(paymentSubmitResult{
			PaymentID: flowTestInt64(args["paymentId"]),
			Status:    "pending",
			Reference: strings.TrimSpace(args["reference"]),
			Channel:   strings.TrimSpace(args["channel"]),
			PayerName: strings.TrimSpace(args["payerName"]),
			Recipient: strings.TrimSpace(args["recipient"]),
		}, "No se declaró ningún pago en la tienda conectada.")

	case "payment_methods_render":
		return flowTestJSON(models.PaymentInstructionsResult{
			Found: true, Count: 1,
			Message: "[Chat de prueba] Aquí se mostrarían los métodos de pago activos de la organización.",
		}, "No se leyeron los métodos de pago reales: el texto es un marcador y no contiene ninguna cuenta.")

	case "subscription_activate":
		return flowTestJSON(models.OrganizationSubscription{
			RecordID:       flowTestSyntheticRecordID(run.nextWriteSequence()),
			ActivationCode: strings.TrimSpace(args["activationCode"]),
			PlanKey:        strings.TrimSpace(args["planKey"]),
			BillingCycle:   strings.TrimSpace(args["billingCycle"]),
			Status:         "activa",
		}, "No se activó ninguna suscripción: no se resolvió el código ni se tocó el ledger comercial.")

	case "credit_recharge_activate":
		return flowTestJSON(models.CreditWallet{},
			"No se acreditó ningún crédito: el monedero devuelto está vacío y no corresponde a ninguna organización.")

	default:
		// Mismo error que el runtime real, para que la rama `error` se comporte
		// igual si alguien publica un toolRef que nadie implementa.
		return "", "", fmt.Errorf("herramienta %q no implementada", ref)
	}
}

// flowTestAgentToolResult devuelve **texto**, no JSON: es lo que ven los
// ejecutores del lado del agente, y darle un objeto al modelo cambiaría cómo lo
// interpreta. Se reutilizan literalmente los mismos mensajes de «sin
// resultados» del camino real para que el modelo lea exactamente lo mismo.
func flowTestAgentToolResult(ref string, config map[string]string) (string, string) {
	switch ref {
	case "data_query":
		objects := splitList(config["objects"])
		if object := strings.TrimSpace(config["object"]); object != "" {
			objects = []string{object}
		}
		return dataQueryEmptyForModel(objects),
			"No se leyó la tabla: al modelo se le entregó el mismo texto que produce una lectura real sin coincidencias."
	case "catalog_search":
		return catalogSearchEmptyForModel,
			"No se consultó la tienda: al modelo se le entregó el mismo texto que produce una búsqueda real sin coincidencias."
	case "catalog_product":
		return catalogProductMissingForModel,
			"No se consultó la tienda: al modelo se le entregó el mismo texto que produce un producto inexistente."
	case "dataset_query":
		return datasetEmptyForModel,
			"No se llamó al dataset externo: al modelo se le entregó el mismo texto que produce una lectura real sin coincidencias."
	default:
		// No puede ocurrir: flowTestAgentTooling ya rechazó lo que no es ForAgent.
		return fmt.Sprintf("La herramienta %q no está disponible.", ref),
			"Herramienta sin simulación disponible."
	}
}

func flowTestJSON(value any, note string) (string, string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", "", err
	}
	return string(raw), note, nil
}

func flowTestOrderLineCount(args map[string]string) int {
	count := 0
	for index := 1; index <= maxOrderLines; index++ {
		if strings.TrimSpace(args["item."+strconv.Itoa(index)+".productId"]) != "" {
			count++
		}
	}
	return count
}

func flowTestInt64(value string) int64 {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

// flowTestArgsFromJSON aplana los argumentos que redactó el modelo para poder
// enseñarlos en el panel con la misma forma que los de un bloque.
func flowTestArgsFromJSON(input json.RawMessage) map[string]string {
	var decoded map[string]any
	if len(input) == 0 || json.Unmarshal(input, &decoded) != nil {
		return map[string]string{}
	}
	args := make(map[string]string, len(decoded))
	for key, value := range decoded {
		if text, ok := value.(string); ok {
			args[key] = text
			continue
		}
		raw, err := json.Marshal(value)
		if err != nil {
			continue
		}
		args[key] = string(raw)
	}
	return args
}
