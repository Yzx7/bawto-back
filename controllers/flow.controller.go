package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/engine"
	authmw "github.com/Yzx7/sacs-chatbots/middlewares/auth"
	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
)

// Flujos del bot (§10.1 del plan). La autorización se resuelve por bot → org con
// botWithRole, igual que el resto del backend: no se denormaliza org_id en flows.
//
// En esta fase la escritura del flujo que ejecuta el bot sigue yendo a
// `bots.flow` (PUT /bots/:botId/flow). Estos endpoints construyen el lugar donde
// alojar los flujos `schedule`; §12 paso 7 mueve la escritura más adelante.

// currentUserID devuelve el id del usuario autenticado ("" si no hay claims).
func (con *Controller) currentUserID(c *fiber.Ctx) string {
	if claims, ok := authmw.Current(c); ok {
		return claims.UserID
	}
	return ""
}

// flowWithRole carga el flujo comprobando permisos sobre la org dueña del bot.
func (con *Controller) flowWithRole(c *fiber.Ctx, roles ...string) (*models.Bot, *models.Flow, error) {
	bot, err := con.botWithRole(c, c.Params("botId"), roles...)
	if err != nil {
		return nil, nil, err
	}
	flow, err := models.GetFlow(c.Context(), con.Env.Postgres, bot.ID, c.Params("flowId"))
	if err != nil {
		return nil, nil, fiber.NewError(fiber.StatusInternalServerError, "error obteniendo el flujo")
	}
	if flow == nil {
		return nil, nil, fiber.NewError(fiber.StatusNotFound, "flujo no encontrado")
	}
	return bot, flow, nil
}

// GET /bots/:botId/flows — lista de flujos (cualquier miembro).
func (con *Controller) ListBotFlows(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"))
	if err != nil {
		return con.failErr(c, err)
	}
	flows, err := models.ListFlows(c.Context(), con.Env.Postgres, bot.ID, c.QueryBool("archived", false))
	if err != nil {
		con.Env.Logger.Error("ListFlows", "botId", bot.ID, "err", err.Error())
		return con.fail(c, fiber.StatusInternalServerError, "error obteniendo los flujos")
	}
	return con.ok(c, "ok", flows)
}

// POST /bots/:botId/flows — crea un flujo en borrador (owner/admin/member).
func (con *Controller) CreateBotFlow(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	var b struct {
		Key         string          `json:"key"`
		Name        string          `json:"name"`
		TriggerType string          `json:"triggerType"`
		Priority    int             `json:"priority"`
		IsFallback  bool            `json:"isFallback"`
		Draft       json.RawMessage `json:"draft"`
	}
	if err := c.BodyParser(&b); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido")
	}
	b.Key = strings.TrimSpace(strings.ToLower(b.Key))
	b.Name = strings.TrimSpace(b.Name)
	b.TriggerType = strings.TrimSpace(b.TriggerType)
	if b.Name == "" {
		return con.fail(c, fiber.StatusBadRequest, "el nombre es obligatorio")
	}
	if !models.ValidFlowKey(b.Key) {
		return con.fail(c, fiber.StatusBadRequest, models.ErrFlowInvalidKey.Error())
	}
	switch b.TriggerType {
	case "message", "schedule", "event":
	default:
		return con.fail(c, fiber.StatusBadRequest, "triggerType debe ser message, schedule o event")
	}
	if len(b.Draft) > 0 && !json.Valid(b.Draft) {
		return con.fail(c, fiber.StatusBadRequest, "el borrador no es JSON válido")
	}

	flow, err := models.CreateFlow(c.Context(), con.Env.Postgres, bot.ID, models.NewFlow{
		Key:         b.Key,
		Name:        b.Name,
		TriggerType: b.TriggerType,
		Priority:    b.Priority,
		IsFallback:  b.IsFallback,
		Draft:       b.Draft,
		UserID:      con.currentUserID(c),
	})
	if err != nil {
		return con.failFlow(c, "CreateFlow", bot.ID, err, "no se pudo crear el flujo")
	}
	return c.Status(fiber.StatusCreated).JSON(types.OK("flujo creado", flow))
}

// GET /bots/:botId/flows/:flowId — detalle (cualquier miembro).
func (con *Controller) GetBotFlowByID(c *fiber.Ctx) error {
	_, flow, err := con.flowWithRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	return con.ok(c, "ok", flow)
}

// GET /bots/:botId/flows/:flowId/draft — borrador editable.
func (con *Controller) GetBotFlowDraft(c *fiber.Ctx) error {
	_, flow, err := con.flowWithRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	return con.ok(c, "ok", flow.Draft)
}

// PUT /bots/:botId/flows/:flowId/draft — guarda el borrador (owner/admin/member).
// El borrador puede estar incompleto: la validación dura es al publicar.
func (con *Controller) UpdateBotFlowDraft(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	if flow.ArchivedAt != nil {
		return con.fail(c, fiber.StatusConflict, models.ErrFlowArchived.Error())
	}
	body := c.Body()
	if !json.Valid(body) {
		return con.fail(c, fiber.StatusBadRequest, "flujo inválido (no es JSON)")
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return con.fail(c, fiber.StatusBadRequest, "el borrador debe ser un objeto JSON")
	}
	updated, err := models.UpdateFlowDraft(c.Context(), con.Env.Postgres, bot.ID, flow.ID, json.RawMessage(body), con.currentUserID(c))
	if err != nil {
		return con.failFlow(c, "UpdateFlowDraft", bot.ID, err, "no se pudo guardar el borrador")
	}
	if updated == nil {
		return con.fail(c, fiber.StatusNotFound, "flujo no encontrado")
	}
	return con.ok(c, "borrador guardado", updated)
}

// POST /bots/:botId/flows/:flowId/validate — valida el grafo sin publicar.
// Acepta un grafo en el cuerpo; si viene vacío, valida el borrador guardado.
func (con *Controller) ValidateBotFlow(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	raw := flow.Draft
	if body := c.Body(); len(body) > 0 {
		raw = json.RawMessage(body)
	}
	if problem := validateFlowDefinition(raw, flow.TriggerType); problem != "" {
		return con.ok(c, "el flujo tiene errores", fiber.Map{"valid": false, "error": problem})
	}
	templateValidation, err := models.ValidateFlowTemplates(c.Context(), con.Env.Postgres, bot.ID, raw)
	if err != nil {
		return con.ok(c, "el flujo tiene errores", fiber.Map{"valid": false, "error": err.Error()})
	}
	return con.ok(c, "el flujo es válido", fiber.Map{"valid": true, "warnings": templateValidation.Warnings})
}

// POST /bots/:botId/flows/:flowId/publish — publica el borrador (owner/admin/member).
//
// Secuencia de §5.2: validar → normalizar → checksum → no-op si coincide con lo
// publicado → versión inmutable → published_version_id.
func (con *Controller) PublishBotFlow(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	if flow.ArchivedAt != nil {
		return con.fail(c, fiber.StatusConflict, models.ErrFlowArchived.Error())
	}
	if problem := validateFlowDefinition(flow.Draft, flow.TriggerType); problem != "" {
		return con.fail(c, fiber.StatusBadRequest, problem)
	}
	var templateWarnings []string
	if flow.TriggerType == "schedule" {
		// Meta es la fuente de verdad: publicar fuerza una fotografía actual,
		// en vez de confiar en una caché que podría haber quedado obsoleta.
		if _, err := con.syncBotTemplates(c.Context(), bot); err != nil {
			return con.fail(c, fiber.StatusBadGateway, "no se pudo confirmar las plantillas con Meta: "+err.Error())
		}
		templateValidation, err := models.ValidateFlowTemplates(c.Context(), con.Env.Postgres, bot.ID, flow.Draft)
		if err != nil {
			return con.fail(c, fiber.StatusBadRequest, err.Error())
		}
		templateWarnings = templateValidation.Warnings
	}
	definition, checksum, err := engine.CanonicalChecksum(flow.Draft)
	if err != nil {
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}
	res, err := models.PublishFlow(c.Context(), con.Env.Postgres, bot.ID, flow.ID, definition, checksum, con.currentUserID(c))
	if err != nil {
		return con.failFlow(c, "PublishFlow", bot.ID, err, "no se pudo publicar el flujo")
	}
	if res == nil {
		return con.fail(c, fiber.StatusNotFound, "flujo no encontrado")
	}
	res.Warnings = templateWarnings
	msg := "flujo publicado"
	if !res.Created {
		msg = "sin cambios: el grafo ya estaba publicado en esta versión"
	}
	return con.ok(c, msg, res)
}

// POST /bots/:botId/flows/:flowId/pause — deja de crear ejecuciones nuevas.
func (con *Controller) PauseBotFlow(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	updated, err := models.PauseFlow(c.Context(), con.Env.Postgres, bot.ID, flow.ID, con.currentUserID(c))
	if err != nil {
		return con.failFlow(c, "PauseFlow", bot.ID, err, "no se pudo pausar el flujo")
	}
	if updated == nil {
		return con.fail(c, fiber.StatusConflict, "el flujo está archivado")
	}
	return con.ok(c, "flujo pausado", updated)
}

// POST /bots/:botId/flows/:flowId/resume — reactiva un flujo pausado.
func (con *Controller) ResumeBotFlow(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	updated, err := models.ResumeFlow(c.Context(), con.Env.Postgres, bot.ID, flow.ID, con.currentUserID(c))
	if err != nil {
		return con.failFlow(c, "ResumeFlow", bot.ID, err, "no se pudo reactivar el flujo")
	}
	if updated == nil {
		return con.fail(c, fiber.StatusConflict, "solo se puede reactivar un flujo pausado con versión publicada")
	}
	return con.ok(c, "flujo reactivado", updated)
}

// POST /bots/:botId/flows/:flowId/archive — archiva y libera la key (§5.1).
func (con *Controller) ArchiveBotFlow(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin")
	if err != nil {
		return con.failErr(c, err)
	}
	updated, err := models.ArchiveFlow(c.Context(), con.Env.Postgres, bot.ID, flow.ID, con.currentUserID(c))
	if err != nil {
		return con.failFlow(c, "ArchiveFlow", bot.ID, err, "no se pudo archivar el flujo")
	}
	if updated == nil {
		return con.ok(c, "el flujo ya estaba archivado", flow)
	}
	return con.ok(c, "flujo archivado", updated)
}

// GET /bots/:botId/flows/:flowId/versions — historial de versiones publicadas.
func (con *Controller) ListBotFlowVersions(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	versions, err := models.ListFlowVersions(c.Context(), con.Env.Postgres, bot.ID, flow.ID)
	if err != nil {
		return con.failFlow(c, "ListFlowVersions", bot.ID, err, "no se pudieron obtener las versiones")
	}
	return con.ok(c, "ok", versions)
}

// POST /bots/:botId/flows/:flowId/versions/:versionId/restore — copia una versión
// al borrador. No publica: republicar es un acto explícito y crea una versión
// nueva (por eso flow_versions no tiene UNIQUE por checksum).
func (con *Controller) RestoreBotFlowVersion(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	if flow.ArchivedAt != nil {
		return con.fail(c, fiber.StatusConflict, models.ErrFlowArchived.Error())
	}
	updated, err := models.RestoreFlowVersion(c.Context(), con.Env.Postgres, bot.ID, flow.ID,
		c.Params("versionId"), con.currentUserID(c))
	if err != nil {
		return con.failFlow(c, "RestoreFlowVersion", bot.ID, err, "no se pudo restaurar la versión")
	}
	if updated == nil {
		return con.fail(c, fiber.StatusNotFound, "versión no encontrada")
	}
	return con.ok(c, "versión restaurada en el borrador", updated)
}

// validateFlowDefinition reutiliza engine.Validate (la única fuente de verdad de
// qué grafo puede ejecutar el motor). Devuelve "" si el grafo es válido.
func validateFlowDefinition(raw json.RawMessage, triggerType string) string {
	if len(raw) == 0 {
		return "el flujo está vacío"
	}
	var flow engine.Flow
	if err := json.Unmarshal(raw, &flow); err != nil {
		return "flujo inválido (no es JSON del editor)"
	}
	if triggerType != "" && flow.Trigger.Type != triggerType {
		return "el trigger del grafo (" + flow.Trigger.Type + ") no coincide con el del flujo (" + triggerType + ")"
	}
	if err := engine.Validate(&flow); err != nil {
		return err.Error()
	}
	return ""
}

// failFlow traduce los errores de dominio de models/flow.go a códigos HTTP.
func (con *Controller) failFlow(c *fiber.Ctx, op, botID string, err error, fallback string) error {
	switch {
	case errors.Is(err, models.ErrFlowKeyTaken), errors.Is(err, models.ErrFlowFallbackTaken):
		return con.fail(c, fiber.StatusConflict, err.Error())
	case errors.Is(err, models.ErrFlowArchived):
		return con.fail(c, fiber.StatusConflict, err.Error())
	case errors.Is(err, models.ErrFlowInvalidKey):
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}
	con.Env.Logger.Error(op, "botId", botID, "err", err.Error())
	return con.fail(c, fiber.StatusInternalServerError, fallback)
}

// messageFlowDefinition resuelve el grafo `message` a ejecutar en el webhook:
// la versión publicada del flujo `message` de mayor prioridad.
//
// Sin versión publicada no hay grafo, y el webhook cae al eco. Es deliberado:
// pausar o despublicar un flujo tiene que dejar de ejecutarlo, no revivir una
// copia paralela.
func (con *Controller) messageFlowDefinition(ctx context.Context, botID string) json.RawMessage {
	def, err := models.PublishedFlowDefinition(ctx, con.Env.Postgres, botID, "message")
	if err != nil {
		con.Env.Logger.Error("PublishedFlowDefinition", "botId", botID, "err", err.Error())
		return nil
	}
	return def
}
