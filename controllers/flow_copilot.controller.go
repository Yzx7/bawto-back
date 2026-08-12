package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"

	"github.com/Yzx7/sacs-chatbots/authoring"
	"github.com/Yzx7/sacs-chatbots/copilot"
	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
)

func (con *Controller) flowCopilotCapability() models.FlowCopilotCapability {
	reasonCode := "feature_disabled"
	if con != nil && con.Env != nil && con.Env.Config != nil {
		if ready, reason := con.Env.Config.CopilotReadiness(); ready {
			// La configuración por sí sola no crea el runner. Hasta conectar el
			// servicio, no se aceptan sesiones que quedarían huérfanas.
			return models.FlowCopilotCapability{
				Enabled: false, ProviderOperational: false,
				Reason: "El proveedor está configurado, pero el runner de autoría todavía no está disponible.",
			}
		} else {
			reasonCode = reason
		}
	}
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
	reason := reasons[reasonCode]
	if reason == "" {
		reason = "El Copilot de autoría no está disponible."
	}
	return models.FlowCopilotCapability{Enabled: false, Reason: reason, ProviderOperational: false}
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
	var body struct {
		PersistedDraftChecksum string          `json:"persistedDraftChecksum"`
		WorkingDraft           json.RawMessage `json:"workingDraft"`
		EditorRevision         string          `json:"editorRevision"`
		SelectedNodeID         string          `json:"selectedNodeId"`
	}
	if err := decodeStrictCopilotBody(c.Body(), &body); err != nil {
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
// El contrato queda reservado como JSON/NDJSON, pero no se crea una fila
// running hasta que exista un runner capaz de cancelarla y finalizarla.
func (con *Controller) CreateBotFlowCopilotTurn(c *fiber.Ctx) error {
	if _, _, err := con.flowWithRole(c, "owner", "admin", "member"); err != nil {
		return con.failErr(c, err)
	}
	return con.failCopilotUnavailable(c)
}

// POST /bots/:botId/flows/:flowId/copilot/turns/:turnId/cancel
func (con *Controller) CancelBotFlowCopilotTurn(c *fiber.Ctx) error {
	// Cancelar solo reduce efectos/coste y sigue aislado por created_by; una
	// persona degradada a viewer conserva la posibilidad de detener su turno.
	bot, flow, err := con.flowWithRole(c)
	if err != nil {
		return con.failErr(c, err)
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
	if err := decodeStrictCopilotBody(c.Body(), &body); err != nil {
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
	if err := decodeStrictCopilotBody(c.Body(), &body); err != nil {
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

func decodeStrictCopilotBody(body []byte, target any) error {
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
