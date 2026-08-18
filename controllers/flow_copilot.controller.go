package controllers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yzx7/sacs-chatbots/authoring"
	"github.com/Yzx7/sacs-chatbots/copilot"
	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
)

func (con *Controller) flowCopilotCapability() models.FlowCopilotCapability {
	if con == nil || con.Env == nil || con.Env.Config == nil {
		return models.FlowCopilotCapability{Enabled: false, Reason: "El Copilot de autoría no está disponible.", ProviderOperational: false}
	}
	ready, reasonCode := con.Env.Config.CopilotReadiness()
	if !ready {
		return models.FlowCopilotCapability{Enabled: false, Reason: copilotReadinessReason(reasonCode), ProviderOperational: false}
	}
	if con.Env.Copilot == nil || con.Env.Copilot.Agent == nil {
		// La configuración está completa pero el proceso no construyó el runner;
		// no se aceptan sesiones que quedarían huérfanas.
		return models.FlowCopilotCapability{
			Enabled: false, ProviderOperational: false,
			Reason: "El proveedor está configurado, pero el runner de autoría todavía no está disponible.",
		}
	}
	return models.FlowCopilotCapability{Enabled: true, ProviderOperational: true}
}

func copilotReadinessReason(code string) string {
	reasons := map[string]string{
		"feature_disabled":         "El Copilot de autoría todavía no está habilitado.",
		"missing_api_key":          "El Copilot no tiene una credencial de proveedor configurada.",
		"missing_provider":         "El Copilot no tiene proveedor configurado.",
		"missing_model":            "El Copilot no tiene modelo de autoría configurado.",
		"missing_base_url":         "El Copilot no tiene endpoint de proveedor configurado.",
		"missing_reasoning_effort": "El Copilot no tiene nivel de razonamiento configurado.",
		"invalid_max_steps":        "El presupuesto de pasos del Copilot es inválido.",
		"invalid_timeout":          "El timeout del Copilot es inválido.",
	}
	if reason, ok := reasons[code]; ok {
		return reason
	}
	return "El Copilot de autoría no está disponible."
}

func (con *Controller) failCopilotUnavailable(c *fiber.Ctx) error {
	capability := con.flowCopilotCapability()
	return c.Status(fiber.StatusServiceUnavailable).JSON(types.ErrData(capability.Reason, capability))
}

// POST /bots/:botId/flows/:flowId/copilot/sessions
func (con *Controller) CreateBotFlowCopilotSession(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	if !con.flowCopilotCapability().Enabled {
		return con.failCopilotUnavailable(c)
	}
	wallet, err := models.GetOrCreateCreditWallet(c.Context(), con.Env.Postgres, bot.OrgID)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo comprobar el saldo de créditos")
	}
	if wallet.Balance <= 0 {
		return con.fail(c, fiber.StatusPaymentRequired, "saldo de créditos agotado; recarga antes de usar el Copilot")
	}
	var body struct {
		PersistedDraftChecksum string          `json:"persistedDraftChecksum"`
		WorkingDraft           json.RawMessage `json:"workingDraft"`
		EditorRevision         string          `json:"editorRevision"`
		SelectedNodeID         string          `json:"selectedNodeId"`
	}
	if err := decodeStrictJSONBody(c.Body(), &body); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido: "+err.Error())
	}
	if len(body.SelectedNodeID) > 200 {
		return con.fail(c, fiber.StatusBadRequest, "selectedNodeId excede 200 caracteres")
	}
	session, err := models.CreateFlowCopilotSession(c.Context(), con.Env.Postgres,
		models.CreateFlowCopilotSessionParams{
			OrganizationID: bot.OrgID, BotID: bot.ID, FlowID: flow.ID,
			CreatedBy: con.currentUserID(c), PersistedDraftChecksum: body.PersistedDraftChecksum,
			WorkingDraft: body.WorkingDraft, EditorRevision: body.EditorRevision,
		})
	if err != nil {
		return con.failFlowCopilot(c, "CreateFlowCopilotSession", bot.ID, err)
	}
	if session == nil {
		return con.fail(c, fiber.StatusNotFound, "flujo no encontrado")
	}
	return c.Status(fiber.StatusCreated).JSON(types.OK("sesión del Copilot creada", session))
}

// GET /bots/:botId/flows/:flowId/copilot/sessions
func (con *Controller) ListBotFlowCopilotSessions(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	sessions, err := models.ListFlowCopilotSessions(c.Context(), con.Env.Postgres,
		bot.OrgID, bot.ID, flow.ID, con.currentUserID(c), strings.TrimSpace(c.Query("status")))
	if err != nil {
		return con.failFlowCopilot(c, "ListFlowCopilotSessions", bot.ID, err)
	}
	return con.ok(c, "ok", sessions)
}

// GET /bots/:botId/flows/:flowId/copilot/sessions/:sessionId
func (con *Controller) GetBotFlowCopilotSession(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	detail, err := models.GetFlowCopilotSession(c.Context(), con.Env.Postgres,
		bot.OrgID, bot.ID, flow.ID, c.Params("sessionId"), con.currentUserID(c))
	if err != nil {
		return con.failFlowCopilot(c, "GetFlowCopilotSession", bot.ID, err)
	}
	if detail == nil {
		return con.fail(c, fiber.StatusNotFound, "sesión no encontrada")
	}
	return con.ok(c, "ok", detail)
}

// POST /bots/:botId/flows/:flowId/copilot/sessions/:sessionId/turns
// Ejecuta un turno del agente de autoría y lo retransmite como NDJSON. El
// modelo solo puede producir una propuesta; aplicar, descartar y deshacer
// siguen siendo acciones humanas en endpoints separados.
func (con *Controller) CreateBotFlowCopilotTurn(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	if !con.flowCopilotCapability().Enabled {
		return con.failCopilotUnavailable(c)
	}
	var body struct {
		Message                   string          `json:"message"`
		PersistedDraftChecksum    string          `json:"persistedDraftChecksum"`
		WorkingDraft              json.RawMessage `json:"workingDraft"`
		EditorRevision            string          `json:"editorRevision"`
		SelectedNodeID            string          `json:"selectedNodeId"`
		ReviseProposalID          string          `json:"reviseProposalId"`
		ExpectedCandidateChecksum string          `json:"expectedCandidateChecksum"`
	}
	if err := decodeStrictJSONBody(c.Body(), &body); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido: "+err.Error())
	}
	if len(body.SelectedNodeID) > 200 {
		return con.fail(c, fiber.StatusBadRequest, "selectedNodeId excede 200 caracteres")
	}

	turn, err := models.CreateFlowCopilotTurn(c.Context(), con.Env.Postgres,
		models.CreateFlowCopilotTurnParams{
			OrganizationID: bot.OrgID, BotID: bot.ID, FlowID: flow.ID,
			SessionID: c.Params("sessionId"), CreatedBy: con.currentUserID(c),
			Message: body.Message, PersistedDraftChecksum: body.PersistedDraftChecksum,
			WorkingDraft: body.WorkingDraft, EditorRevision: body.EditorRevision,
			ReviseProposalID: body.ReviseProposalID, ExpectedCandidateChecksum: body.ExpectedCandidateChecksum,
		})
	if err != nil {
		return con.failFlowCopilot(c, "CreateFlowCopilotTurn", bot.ID, err)
	}
	if turn == nil {
		return con.fail(c, fiber.StatusNotFound, "sesión no encontrada")
	}

	// Los recursos se cargan antes de abrir el stream: si fallan, todavía se
	// puede responder un error JSON normal y se libera el turno reservado.
	resources, err := copilot.LoadResourceBundle(c.Context(), con.Env.Postgres, bot.OrgID, bot.ID)
	if err != nil {
		_, _ = models.FailFlowCopilotTurn(context.WithoutCancel(c.Context()), con.Env.Postgres,
			bot.OrgID, bot.ID, flow.ID, turn.SessionID, turn.ID, con.currentUserID(c), "resource_load_failed")
		return con.failFlow(c, "LoadResourceBundle", bot.ID, err, "no se pudieron cargar los recursos para el Copilot")
	}

	c.Set("Content-Type", "application/x-ndjson")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	// Cargar historial de turnos previos completados en esta sesión para dar contexto conversacional continuo
	var recentConversation []copilot.ConversationEntry
	turnRows, err := con.Env.Postgres.Query(c.Context(),
		`SELECT user_message, assistant_message FROM flow_copilot_turns
		 WHERE session_id=$1::uuid AND status='completed' AND id != $2::uuid
		 ORDER BY sequence ASC LIMIT 20`,
		turn.SessionID, turn.ID)
	if err == nil {
		for turnRows.Next() {
			var umsg string
			var amsg *string
			if err := turnRows.Scan(&umsg, &amsg); err == nil {
				if strings.TrimSpace(umsg) != "" {
					recentConversation = append(recentConversation, copilot.ConversationEntry{Role: "user", Content: umsg})
				}
				if amsg != nil && strings.TrimSpace(*amsg) != "" {
					recentConversation = append(recentConversation, copilot.ConversationEntry{Role: "assistant", Content: *amsg})
				}
			}
		}
		turnRows.Close()
	}

	// Capturas para el goroutine del stream: `c` se recicla en cuanto vuelve el
	// handler, así que nada de lo que se use dentro puede referenciarlo.
	stream := copilotTurnStream{
		pool:           con.Env.Postgres,
		logger:         con.Env.Logger,
		runner:         con.Env.Copilot,
		organizationID: bot.OrgID,
		botID:          bot.ID,
		flowID:         flow.ID,
		sessionID:      turn.SessionID,
		userID:         con.currentUserID(c),
		turn:           *turn,
		persistedBase:  append(json.RawMessage(nil), flow.Draft...),
		request: copilot.TurnRequest{
			Scope:                  copilot.TurnScope{OrganizationID: bot.OrgID, BotID: bot.ID, FlowID: flow.ID},
			UserRequest:            turn.UserMessage,
			CurrentFlow:            append(json.RawMessage(nil), body.WorkingDraft...),
			SelectedNodeID:         body.SelectedNodeID,
			PersistedDraftChecksum: turn.PersistedDraftChecksum,
			EditorRevision:         turn.EditorRevision,
			RecentConversation:     recentConversation,
			Resources:              *resources,
		},
	}

	c.Context().SetBodyStreamWriter(stream.write)
	return nil
}

// POST /bots/:botId/flows/:flowId/copilot/turns/:turnId/cancel
func (con *Controller) CancelBotFlowCopilotTurn(c *fiber.Ctx) error {
	// Cancelar solo reduce efectos/coste y sigue aislado por created_by; una
	// persona degradada a viewer conserva la posibilidad de detener su turno.
	bot, flow, err := con.flowWithRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	// Corta la llamada al proveedor si el turno corre en esta instancia; el
	// estado definitivo lo fija PostgreSQL abajo.
	if con.Env.Copilot != nil {
		con.Env.Copilot.CancelTurn(c.Params("turnId"))
	}
	turn, err := models.CancelFlowCopilotTurn(c.Context(), con.Env.Postgres,
		bot.OrgID, bot.ID, flow.ID, c.Params("turnId"), con.currentUserID(c))
	if err != nil {
		return con.failFlowCopilot(c, "CancelFlowCopilotTurn", bot.ID, err)
	}
	if turn == nil {
		return con.fail(c, fiber.StatusNotFound, "turno no encontrado")
	}
	return con.ok(c, "turno cancelado", turn)
}

// POST /bots/:botId/flows/:flowId/copilot/proposals/:proposalId/apply
func (con *Controller) ApplyBotFlowCopilotProposal(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	var body struct {
		ExpectedDraftChecksum  string          `json:"expectedDraftChecksum"`
		ExpectedEditorRevision string          `json:"expectedEditorRevision"`
		CurrentWorkingDraft    json.RawMessage `json:"currentWorkingDraft"`
	}
	if err := decodeStrictJSONBody(c.Body(), &body); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido: "+err.Error())
	}
	result, err := models.ApplyFlowCopilotProposal(c.Context(), con.Env.Postgres,
		models.ApplyFlowCopilotProposalParams{
			OrganizationID: bot.OrgID, BotID: bot.ID, FlowID: flow.ID,
			ProposalID: c.Params("proposalId"), CreatedBy: con.currentUserID(c),
			ExpectedDraftChecksum:  body.ExpectedDraftChecksum,
			ExpectedEditorRevision: body.ExpectedEditorRevision,
			CurrentWorkingDraft:    body.CurrentWorkingDraft,
			ValidateCandidate:      validateFlowCopilotCandidate,
		})
	if err != nil {
		return con.failFlowCopilot(c, "ApplyFlowCopilotProposal", bot.ID, err)
	}
	if result == nil {
		return con.fail(c, fiber.StatusNotFound, "propuesta no encontrada")
	}
	return con.ok(c, "propuesta aplicada al borrador; no se publicó", result)
}

// POST /bots/:botId/flows/:flowId/copilot/proposals/:proposalId/dismiss
func (con *Controller) DismissBotFlowCopilotProposal(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	proposal, err := models.DismissFlowCopilotProposal(c.Context(), con.Env.Postgres,
		bot.OrgID, bot.ID, flow.ID, c.Params("proposalId"), con.currentUserID(c))
	if err != nil {
		return con.failFlowCopilot(c, "DismissFlowCopilotProposal", bot.ID, err)
	}
	if proposal == nil {
		return con.fail(c, fiber.StatusNotFound, "propuesta no encontrada")
	}
	return con.ok(c, "propuesta descartada", proposal)
}

// POST /bots/:botId/flows/:flowId/copilot/proposals/:proposalId/undo
func (con *Controller) UndoBotFlowCopilotProposal(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	var body struct {
		ExpectedDraftChecksum string `json:"expectedDraftChecksum"`
	}
	if err := decodeStrictJSONBody(c.Body(), &body); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido: "+err.Error())
	}
	result, err := models.UndoFlowCopilotProposal(c.Context(), con.Env.Postgres,
		models.UndoFlowCopilotProposalParams{
			OrganizationID: bot.OrgID, BotID: bot.ID, FlowID: flow.ID,
			ProposalID: c.Params("proposalId"), CreatedBy: con.currentUserID(c),
			ExpectedDraftChecksum: body.ExpectedDraftChecksum,
		})
	if err != nil {
		return con.failFlowCopilot(c, "UndoFlowCopilotProposal", bot.ID, err)
	}
	if result == nil {
		return con.fail(c, fiber.StatusNotFound, "propuesta no encontrada")
	}
	return con.ok(c, "cambios del Copilot deshechos en el borrador; no se publicó", result)
}

// POST /bots/:botId/flows/:flowId/copilot/sessions/:sessionId/close
func (con *Controller) CloseBotFlowCopilotSession(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	session, err := models.CloseFlowCopilotSession(c.Context(), con.Env.Postgres,
		bot.OrgID, bot.ID, flow.ID, c.Params("sessionId"), con.currentUserID(c))
	if err != nil {
		return con.failFlowCopilot(c, "CloseFlowCopilotSession", bot.ID, err)
	}
	if session == nil {
		return con.fail(c, fiber.StatusNotFound, "sesión no encontrada")
	}
	return con.ok(c, "sesión cerrada", session)
}

func (con *Controller) failFlowCopilot(c *fiber.Ctx, op, botID string, err error) error {
	var conflict *models.FlowCopilotConflictError
	var input *models.FlowCopilotInputError
	if errors.As(err, &conflict) {
		return c.Status(fiber.StatusConflict).JSON(types.ErrData(conflict.Error(), conflict))
	}
	if errors.As(err, &input) {
		return con.fail(c, fiber.StatusBadRequest, input.Error())
	}
	return con.failFlow(c, op, botID, err, "no se pudo completar la operación del Copilot")
}

func decodeStrictJSONBody(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("solo se permite un objeto JSON")
		}
		return err
	}
	return nil
}

func validateFlowCopilotCandidate(
	ctx context.Context,
	tx pgx.Tx,
	organizationID, botID string,
	raw json.RawMessage,
) error {
	safe, err := models.LoadFlowCopilotResourcesSafe(ctx, tx, organizationID, botID)
	if err != nil {
		return err
	}
	bundle, err := copilot.BuildResourceBundle(safe)
	if err != nil {
		return err
	}
	report := authoring.ValidateForAuthoring(raw, bundle.Snapshot)
	if !report.HasErrors() {
		return nil
	}
	problems := make([]string, 0)
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Severity == authoring.SeverityError {
			problems = append(problems, diagnostic.Message)
		}
	}
	return &models.FlowValidationError{Problem: strings.Join(problems, "; ")}
}

// copilotTurnStream retransmite un turno como NDJSON. Todo el trabajo ocurre
// dentro del body stream writer: el modelo puede tardar decenas de segundos y
// Fiber necesita que el handler devuelva el control de inmediato.
type copilotTurnStream struct {
	pool           *pgxpool.Pool
	logger         *slog.Logger
	runner         *copilot.Runner
	organizationID string
	botID          string
	flowID         string
	sessionID      string
	userID         string
	turn           models.FlowCopilotTurn
	persistedBase  json.RawMessage
	request        copilot.TurnRequest
}

func (s copilotTurnStream) write(w *bufio.Writer) {
	emit := func(event any) bool {
		raw, err := json.Marshal(event)
		if err != nil {
			return false
		}
		if _, err := w.Write(raw); err != nil {
			return false
		}
		if _, err := w.WriteString("\n"); err != nil {
			return false
		}
		return w.Flush() == nil
	}

	_ = emit(map[string]any{"type": "turn.started", "turnId": s.turn.ID})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.runner.RegisterCancel(s.turn.ID, cancel)
	defer s.runner.UnregisterCancel(s.turn.ID)

	runCtx := copilot.WithActivitySink(ctx, s.activitySink(emit))
	runCtx = copilot.WithUsageRecorder(runCtx, s.usageRecorder())
	runCtx = copilot.WithDeltaSink(runCtx, s.deltaSink(emit))

	result, runErr := s.runner.Agent.RunTurn(runCtx, s.request)
	if runErr != nil {
		if errors.Is(runErr, context.DeadlineExceeded) {
			result = &copilot.TurnResult{
				Terminal: copilot.SubmitProposalInput{
					Mode:     copilot.TerminalQuestion,
					Response: "El procesamiento de este turno superó el tiempo límite configurado mientras diseñaba el flujo. ¿Deseas que continúe en el siguiente mensaje?",
					Warnings: []string{"Se alcanzó el tiempo límite de ejecución en este turno."},
				},
			}
		} else {
			s.logger.Error("copilot: turno fallido", "turnId", s.turn.ID, "err", runErr.Error())
			code := copilotTurnErrorCode(runErr)
			_, _ = models.FailFlowCopilotTurn(context.Background(), s.pool,
				s.organizationID, s.botID, s.flowID, s.sessionID, s.turn.ID, s.userID, code)
			_ = emit(map[string]any{"type": "turn.failed", "turnId": s.turn.ID, "code": code, "message": copilotTurnErrorMessage(code)})
			return
		}
	}

	completed, completeErr := s.completeTurn(result)
	if completeErr != nil {
		s.logger.Error("copilot: completar turno", "turnId", s.turn.ID, "err", completeErr.Error())
		_, _ = models.FailFlowCopilotTurn(context.Background(), s.pool,
			s.organizationID, s.botID, s.flowID, s.sessionID, s.turn.ID, s.userID, "persist_failed")
		_ = emit(map[string]any{"type": "turn.failed", "turnId": s.turn.ID, "code": "persist_failed", "message": copilotTurnErrorMessage("persist_failed")})
		return
	}

	_ = emit(map[string]any{"type": "assistant.message", "turnId": s.turn.ID, "message": result.Terminal.Response})
	if completed != nil && completed.Proposal != nil {
		_ = emit(map[string]any{"type": "proposal.ready", "proposal": completed.Proposal})
	}
	finalTurn := s.turn
	if completed != nil && completed.Turn != nil {
		finalTurn = *completed.Turn
	}
	_ = emit(map[string]any{"type": "turn.completed", "turnId": s.turn.ID, "turn": finalTurn, "costUsd": result.Usage.CostUSD})
}

// deltaSink retransmite el avance del modelo dentro de cada paso. Es lo que
// permite ver el razonamiento formándose en vez de esperar al paso completo:
// el primer thinking_delta de DeepSeek llega a los ~380 ms, mientras que el
// paso entero tarda decenas de segundos.
//
// Corre en la misma goroutine que el escritor del cuerpo, así que no necesita
// sincronización, pero tampoco puede bloquear: cada espera aquí frena al modelo.
func (s copilotTurnStream) deltaSink(emit func(any) bool) func(copilot.StreamDelta) {
	return func(delta copilot.StreamDelta) {
		switch delta.Kind {
		case "thinking":
			_ = emit(map[string]any{
				"type": "thought.delta", "turnId": s.turn.ID,
				"step": delta.Step, "content": delta.Content,
			})
		case "text":
			_ = emit(map[string]any{
				"type": "assistant.delta", "turnId": s.turn.ID,
				"step": delta.Step, "content": delta.Content,
			})
		case "tool":
			// El bloque tool_use se abrió: se anuncia la intención antes de que
			// el paso termine. La ejecución real la reporta activity.*.
			_ = emit(map[string]any{
				"type": "activity.pending", "turnId": s.turn.ID,
				"step": delta.Step, "label": copilotActivityLabel(delta.ToolName),
			})
		}
	}
}

// activitySink traduce ActivityEvent del loop a los eventos activityId/label
// que espera el frontend. Solo viajan nombres permitidos y ciclo de vida,
// nunca argumentos ni resultados.
func (s copilotTurnStream) activitySink(emit func(any) bool) func(copilot.ActivityEvent) {
	activityIDs := make(map[string]string)
	counter := 0
	return func(event copilot.ActivityEvent) {
		key := event.Name + "@" + strconv.Itoa(event.Step)
		if event.Phase == "started" {
			counter++
			id := "act-" + strconv.Itoa(counter)
			activityIDs[key] = id
			_ = emit(map[string]any{"type": "activity.started", "activityId": id,
				"label": copilotActivityLabel(event.Name), "step": event.Step})
			return
		}
		id, ok := activityIDs[key]
		if !ok {
			return
		}
		delete(activityIDs, key)
		_ = emit(map[string]any{"type": "activity.finished", "activityId": id,
			"status": event.Status, "durationMs": event.DurationMs})
	}
}

// usageRecorder registra una fila de ai_usage_events por cada respuesta real del
// proveedor, con purpose flow_authoring y sin chat ni mensaje entrante. El loop
// lo invoca justo tras cada Next exitoso, antes de interpretar la salida.
func (s copilotTurnStream) usageRecorder() func(copilot.ModelUsage) {
	return func(usage copilot.ModelUsage) {
		metadata, _ := json.Marshal(map[string]any{"source": "flow_copilot", "turnId": s.turn.ID, "step": usage.Step})
		err := models.RecordAIUsageAndChargeCredits(context.Background(), s.pool, models.AIUsageChargeInput{
			CreditType:     models.CreditTxAICopilotUsage,
			IdempotencyKey: fmt.Sprintf("ai-copilot:%s:%d", s.turn.ID, usage.Step),
			Notes:          fmt.Sprintf("Copilot autoría turno=%s bot=%s paso=%d", s.turn.ID, s.botID, usage.Step),
			Usage: models.AIUsageEventInput{
				OrganizationID:           s.organizationID,
				BotID:                    s.botID,
				Purpose:                  "flow_authoring",
				Provider:                 usage.Provider,
				Model:                    usage.Model,
				ProviderRequestID:        usage.ProviderRequestID,
				InputTokens:              usage.InputTokens,
				OutputTokens:             usage.OutputTokens,
				CacheReadInputTokens:     usage.CacheReadInputTokens,
				CacheCreationInputTokens: usage.CacheWriteInputTokens,
				InputUSDPerMillion:       usage.InputCostPerMillionUSD,
				OutputUSDPerMillion:      usage.OutputCostPerMillionUSD,
				CacheReadUSDPerMillion:   usage.CacheReadCostPerMillionUSD,
				CacheWriteUSDPerMillion:  usage.CacheWriteCostPerMillionUSD,
				Outcome:                  "ok",
				Metadata:                 metadata,
			},
		})
		if err != nil {
			s.logger.Error("copilot: registrar usage y créditos", "turnId", s.turn.ID, "err", err.Error())
		}
	}
}

func (s copilotTurnStream) completeTurn(result *copilot.TurnResult) (*models.CompleteFlowCopilotTurnResult, error) {
	toolTrace, err := copilotJSONArray(result.Trace)
	if err != nil {
		return nil, err
	}
	playbooks, err := copilotJSONArray(result.Terminal.Playbooks)
	if err != nil {
		return nil, err
	}
	params := models.CompleteFlowCopilotTurnParams{
		OrganizationID: s.organizationID, BotID: s.botID, FlowID: s.flowID,
		SessionID: s.sessionID, TurnID: s.turn.ID, CreatedBy: s.userID,
		AssistantMessage: result.Terminal.Response,
		Mode:             string(result.Terminal.Mode),
		ToolTrace:        toolTrace,
		PlaybookVersions: playbooks,
		CapabilityHash:   authoring.CatalogHash(),
		ResourceHash:     s.request.Resources.Hash,
	}
	if result.Proposal != nil {
		params.CapabilityHash = result.Proposal.CatalogHash
		if result.Proposal.ResourceHash != "" {
			params.ResourceHash = result.Proposal.ResourceHash
		}
		operations, err := copilotJSONArray(result.Proposal.Operations)
		if err != nil {
			return nil, err
		}
		diff, err := json.Marshal(result.Proposal.Diff)
		if err != nil {
			return nil, err
		}
		diagnostics, err := copilotJSONArray(result.Proposal.Diagnostics)
		if err != nil {
			return nil, err
		}
		assumptions, err := copilotJSONArray(result.Terminal.Assumptions)
		if err != nil {
			return nil, err
		}
		requirements, err := copilotJSONArray(result.Terminal.PendingDecisions)
		if err != nil {
			return nil, err
		}
		knowledgeBundleHash := ""
		if len(result.Proposal.KnowledgeBundleHashes) > 0 {
			raw, err := json.Marshal(result.Proposal.KnowledgeBundleHashes)
			if err != nil {
				return nil, err
			}
			knowledgeBundleHash = string(raw)
		}
		params.KnowledgeBundleHash = knowledgeBundleHash
		params.ValidateCandidate = validateFlowCopilotCandidate
		params.Proposal = &models.NewFlowCopilotProposal{
			PersistedBase:       s.persistedBase,
			WorkingBase:         s.request.CurrentFlow,
			EditorRevision:      s.turn.EditorRevision,
			Candidate:           result.Proposal.Candidate,
			Operations:          operations,
			Diff:                diff,
			Assumptions:         assumptions,
			Requirements:        requirements,
			Diagnostics:         diagnostics,
			PlaybookVersions:    playbooks,
			KnowledgeBundleHash: knowledgeBundleHash,
		}
	}
	return models.CompleteFlowCopilotTurn(context.Background(), s.pool, params)
}

// copilotJSONArray serializa asegurando "[]" para colecciones vacías: el
// frontend espera arrays, no null.
func copilotJSONArray(value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if string(raw) == "null" {
		return json.RawMessage("[]"), nil
	}
	return raw, nil
}

func copilotTurnErrorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, copilot.ErrStepBudgetExceeded):
		return "step_budget_exceeded"
	case errors.Is(err, copilot.ErrInvalidTerminal):
		return "invalid_terminal"
	case errors.Is(err, copilot.ErrRepeatedToolCall):
		return "repeated_tool_call"
	default:
		return "provider_error"
	}
}

func copilotTurnErrorMessage(code string) string {
	messages := map[string]string{
		"cancelled":            "El turno se canceló.",
		"timeout":              "El turno superó el tiempo máximo de ejecución.",
		"step_budget_exceeded": "El Copilot agotó el presupuesto de pasos sin completar la propuesta. Prueba pedir el flujo en partes más específicas o aumentar el límite de pasos.",
		"invalid_terminal":     "El Copilot no produjo una respuesta válida para cerrar el turno.",
		"repeated_tool_call":   "El Copilot repitió la misma consulta y se detuvo.",
		"persist_failed":       "No se pudo guardar el resultado del turno en la base de datos.",
		"resource_load_failed": "No se pudieron cargar los recursos del Copilot.",
	}
	if message, ok := messages[code]; ok {
		return message
	}
	return "El proveedor del Copilot devolvió un error."
}

var copilotActivityLabels = map[string]string{
	copilot.ToolGetFlowOutline:         "Leyendo el grafo",
	copilot.ToolGetNodes:               "Leyendo nodos",
	copilot.ToolGetAuthoringCatalog:    "Consultando el catálogo de bloques",
	copilot.ToolGetRuntimeToolCatalog:  "Consultando el catálogo de herramientas",
	copilot.ToolListDataObjects:        "Consultando las tablas de datos",
	copilot.ToolGetDataObjectSchema:    "Consultando el esquema de una tabla",
	copilot.ToolListContactFields:      "Consultando los campos de contacto",
	copilot.ToolListConnectionsSafe:    "Consultando las conexiones",
	copilot.ToolListWhatsAppTemplates:  "Consultando las plantillas",
	copilot.ToolListVariables:          "Consultando las variables",
	copilot.ToolInspectVariablesAtNode: "Analizando las variables del recorrido",
	copilot.ToolListPlaybooks:          "Consultando los playbooks",
	copilot.ToolGetPlaybook:            "Consultando un playbook",
	copilot.ToolValidateCandidate:      "Validando el candidato",
	copilot.ToolApplyFlowOperations:    "Aplicando operaciones al candidato",
	copilot.ToolSubmitProposal:         "Preparando la propuesta",
}

func copilotActivityLabel(name string) string {
	if label, ok := copilotActivityLabels[name]; ok {
		return label
	}
	return name
}
