package controllers

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/scheduler"
	"github.com/Yzx7/sacs-chatbots/types"
)

// Historial y ensayo en seco (§10.2, §10.3 y §11.4 del plan). Es la mitad de la
// interfaz operativa mínima: sin esto no hay forma de saber qué pasó con un
// recordatorio sin abrir la base a mano.

// parseRunFilter traduce la query string a un filtro. Devuelve un *fiber.Error
// cuando el operador escribió algo imposible, en vez de ignorarlo en silencio:
// un filtro mal leído muestra la lista equivocada sin avisar.
func parseRunFilter(c *fiber.Ctx) (models.FlowRunFilter, error) {
	filter := models.FlowRunFilter{
		FlowID:    strings.TrimSpace(c.Query("flowId")),
		ContactID: strings.TrimSpace(c.Query("contactId")),
		RecordID:  strings.TrimSpace(c.Query("recordId")),
		ErrorCode: strings.TrimSpace(c.Query("errorCode")),
		Limit:     c.QueryInt("limit", 50),
		Offset:    c.QueryInt("offset", 0),
	}
	if raw := strings.TrimSpace(c.Query("status")); raw != "" {
		for _, status := range strings.Split(raw, ",") {
			if status = strings.TrimSpace(status); status != "" {
				filter.Statuses = append(filter.Statuses, status)
			}
		}
	}
	for _, spec := range []struct {
		name string
		out  **time.Time
	}{{"from", &filter.From}, {"to", &filter.To}} {
		raw := strings.TrimSpace(c.Query(spec.name))
		if raw == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return filter, fiber.NewError(fiber.StatusBadRequest,
				spec.name+" debe ser una fecha RFC3339 (por ejemplo 2026-07-28T00:00:00Z)")
		}
		*spec.out = &parsed
	}
	if filter.From != nil && filter.To != nil && !filter.To.After(*filter.From) {
		return filter, fiber.NewError(fiber.StatusBadRequest, "el rango de fechas está invertido")
	}
	return filter, nil
}

// GET /bots/:botId/flow-runs — historial paginado con filtros.
func (con *Controller) ListBotFlowRuns(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"))
	if err != nil {
		return con.failErr(c, err)
	}
	filter, err := parseRunFilter(c)
	if err != nil {
		return con.failErr(c, err)
	}
	page, err := models.ListFlowRuns(c.Context(), con.Env.Postgres, bot.ID, filter)
	if err != nil {
		con.Env.Logger.Error("ListFlowRuns", "botId", bot.ID, "err", err.Error())
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo obtener el historial")
	}
	counts, err := models.CountFlowRunsByStatus(c.Context(), con.Env.Postgres, bot.ID, filter)
	if err != nil {
		con.Env.Logger.Error("CountFlowRunsByStatus", "botId", bot.ID, "err", err.Error())
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo obtener el historial")
	}
	return con.ok(c, "ok", fiber.Map{
		"items": page.Items, "total": page.Total, "limit": page.Limit,
		"offset": page.Offset, "countsByStatus": counts,
	})
}

// GET /bots/:botId/flow-runs/:runId — detalle con registro y respuesta.
func (con *Controller) GetBotFlowRun(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"))
	if err != nil {
		return con.failErr(c, err)
	}
	run, err := models.GetFlowRun(c.Context(), con.Env.Postgres, bot.ID, c.Params("runId"))
	if err != nil {
		con.Env.Logger.Error("GetFlowRun", "botId", bot.ID, "err", err.Error())
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo obtener el run")
	}
	if run == nil {
		return con.fail(c, fiber.StatusNotFound, "run no encontrado")
	}
	return con.ok(c, "ok", run)
}

// POST /bots/:botId/flow-runs/:runId/retry — reencola un run terminado.
//
// Un run `unverified` llegó a Meta sin que pudiéramos confirmar el resultado:
// reintentarlo puede duplicar el mensaje al cliente. Por eso exige `confirm`
// explícito y no basta con tener permiso (§11.4).
func (con *Controller) RetryBotFlowRun(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	current, err := models.GetFlowRun(c.Context(), con.Env.Postgres, bot.ID, c.Params("runId"))
	if err != nil {
		con.Env.Logger.Error("GetFlowRun", "botId", bot.ID, "err", err.Error())
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo obtener el run")
	}
	if current == nil {
		return con.fail(c, fiber.StatusNotFound, "run no encontrado")
	}
	if current.Status == "unverified" && !c.QueryBool("confirm", false) {
		return con.fail(c, fiber.StatusConflict,
			"este run quedó sin verificar: reintentarlo puede duplicar el mensaje. Repite con ?confirm=true")
	}
	run, err := models.RequeueFlowRun(c.Context(), con.Env.Postgres, bot.ID,
		c.Params("runId"), con.currentUserID(c))
	if errors.Is(err, models.ErrRunNotRetryable) {
		return con.fail(c, fiber.StatusConflict, err.Error())
	}
	if err != nil {
		con.Env.Logger.Error("RequeueFlowRun", "botId", bot.ID, "err", err.Error())
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo reintentar el run")
	}
	if run == nil {
		return con.fail(c, fiber.StatusNotFound, "run no encontrado")
	}
	return con.ok(c, "run reencolado", run)
}

// POST /bots/:botId/flow-runs/:runId/cancel — cancela un run en espera.
func (con *Controller) CancelBotFlowRun(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = c.BodyParser(&body)
	run, err := models.CancelFlowRunForBot(c.Context(), con.Env.Postgres, bot.ID,
		c.Params("runId"), strings.TrimSpace(body.Reason))
	if errors.Is(err, models.ErrRunNotCancellable) {
		return con.fail(c, fiber.StatusConflict, err.Error())
	}
	if err != nil {
		con.Env.Logger.Error("CancelFlowRun", "botId", bot.ID, "err", err.Error())
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo cancelar el run")
	}
	if run == nil {
		return con.fail(c, fiber.StatusNotFound, "run no encontrado")
	}
	return con.ok(c, "run cancelado", run)
}

// GET /bots/:botId/flows/:flowId/occurrences — últimas pasadas del scheduler.
func (con *Controller) ListBotFlowOccurrences(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	occurrences, err := models.ListScheduleOccurrences(c.Context(), con.Env.Postgres,
		bot.ID, flow.ID, c.QueryInt("limit", 20))
	if err != nil {
		con.Env.Logger.Error("ListScheduleOccurrences", "botId", bot.ID, "err", err.Error())
		return con.fail(c, fiber.StatusInternalServerError, "no se pudieron obtener las ocurrencias")
	}
	return con.ok(c, "ok", occurrences)
}

// POST /bots/:botId/flows/:flowId/schedule/preview — ensayo en seco (§10.2).
//
// No escribe nada. Acepta un grafo en el cuerpo para poder previsualizar lo que
// se está editando; sin cuerpo usa el borrador guardado.
func (con *Controller) PreviewBotFlowSchedule(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	definition := flow.Draft
	if body := c.Body(); len(body) > 0 {
		if !json.Valid(body) {
			return con.fail(c, fiber.StatusBadRequest, "el grafo enviado no es JSON válido")
		}
		definition = json.RawMessage(body)
	}
	at := time.Now().UTC()
	if raw := strings.TrimSpace(c.Query("at")); raw != "" {
		parsed, perr := time.Parse(time.RFC3339, raw)
		if perr != nil {
			return con.fail(c, fiber.StatusBadRequest, "at debe ser una fecha RFC3339")
		}
		at = parsed
	}
	preview, err := scheduler.PreviewFlow(c.Context(), con.Env.Postgres, bot.ID, flow,
		definition, con.Env.Config.SchedulerCatchupWindow, at)
	if err != nil {
		// Casi todo lo que falla aquí es el grafo del operador (cron inválido,
		// vista borrada, timezone inexistente), no un fallo del servidor.
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}
	return con.ok(c, "ok", preview)
}

// POST /bots/:botId/flows/:flowId/duplicate — copia el flujo como borrador.
func (con *Controller) DuplicateBotFlow(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	copied, err := models.DuplicateFlow(c.Context(), con.Env.Postgres, bot.ID, flow.ID, con.currentUserID(c))
	if err != nil {
		return con.failFlow(c, "DuplicateFlow", bot.ID, err, "no se pudo duplicar el flujo")
	}
	if copied == nil {
		return con.fail(c, fiber.StatusNotFound, "flujo no encontrado")
	}
	return c.Status(fiber.StatusCreated).JSON(types.OK("flujo duplicado", copied))
}

// POST /bots/:botId/schedule/validate-cron — valida un cron y devuelve las
// próximas ejecuciones. Cuelga de `schedule` y no de `flows` para no competir
// con `/flows/:flowId`: una key de flujo llamada "validate-cron" es legal.
// No necesita un flujo guardado: el constructor de la UI la usa al escribir.
func (con *Controller) ValidateCronExpression(c *fiber.Ctx) error {
	if _, err := con.botWithRole(c, c.Params("botId")); err != nil {
		return con.failErr(c, err)
	}
	var body struct {
		Cron     string `json:"cron"`
		Timezone string `json:"timezone"`
	}
	if err := c.BodyParser(&body); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido")
	}
	location := time.UTC
	if tz := strings.TrimSpace(body.Timezone); tz != "" {
		loaded, err := time.LoadLocation(tz)
		if err != nil {
			return con.ok(c, "la configuración tiene errores",
				fiber.Map{"valid": false, "error": "timezone desconocida: " + tz})
		}
		location = loaded
	}
	next, err := scheduler.NextOccurrences(strings.TrimSpace(body.Cron), location, time.Now().UTC(), 5)
	if err != nil {
		return con.ok(c, "la configuración tiene errores",
			fiber.Map{"valid": false, "error": "cron inválido: " + err.Error()})
	}
	return con.ok(c, "ok", fiber.Map{"valid": true, "nextOccurrences": next})
}
