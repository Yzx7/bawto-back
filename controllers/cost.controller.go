package controllers

import (
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/models"
)

const maxCostReportRange = 366 * 24 * time.Hour

// GET /orgs/:orgId/costs — consumo agregado de todos los bots de la empresa.
func (con *Controller) GetOrganizationCosts(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID); err != nil {
		return con.failErr(c, err)
	}
	from, to, err := costPeriod(c.Query("from"), c.Query("to"), time.Now().UTC())
	if err != nil {
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}
	report, err := models.GetCostReport(c.Context(), con.Env.Postgres, orgID, "", from, to)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo calcular el consumo")
	}
	return con.ok(c, "consumo calculado", report)
}

// GET /bots/:botId/costs — la misma contabilidad acotada a un bot.
func (con *Controller) GetBotCosts(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"))
	if err != nil {
		return con.failErr(c, err)
	}
	from, to, err := costPeriod(c.Query("from"), c.Query("to"), time.Now().UTC())
	if err != nil {
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}
	report, err := models.GetCostReport(c.Context(), con.Env.Postgres, bot.OrgID, bot.ID, from, to)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo calcular el consumo")
	}
	return con.ok(c, "consumo calculado", report)
}

// GET /bots/:botId/ai-usage — detalle del consumo de tokens del bot. Va aparte
// de /costs porque responde otra pregunta: /costs valoriza la factura, esto
// explica de dónde sale el gasto (qué nodo, qué conversación, cuántos pasos).
func (con *Controller) GetBotAIUsage(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"))
	if err != nil {
		return con.failErr(c, err)
	}
	from, to, err := costPeriod(c.Query("from"), c.Query("to"), time.Now().UTC())
	if err != nil {
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}
	usage, err := models.GetAIUsageAnalytics(c.Context(), con.Env.Postgres, bot.OrgID, bot.ID, from, to)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo calcular el consumo de IA")
	}
	return con.ok(c, "consumo de IA calculado", usage)
}

// costPeriod acepta RFC3339 o YYYY-MM-DD. Una fecha `to` es inclusiva para la
// persona (se convierte internamente en el inicio del día siguiente).
func costPeriod(fromRaw, toRaw string, now time.Time) (time.Time, time.Time, error) {
	now = now.UTC()
	to := now
	var err error
	if strings.TrimSpace(toRaw) != "" {
		to, err = parseCostTime(toRaw, true)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("to inválido: usa YYYY-MM-DD o RFC3339")
		}
	}
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if strings.TrimSpace(fromRaw) != "" {
		from, err = parseCostTime(fromRaw, false)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("from inválido: usa YYYY-MM-DD o RFC3339")
		}
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("from debe ser anterior a to")
	}
	if to.Sub(from) > maxCostReportRange {
		return time.Time{}, time.Time{}, fmt.Errorf("el rango máximo es de 366 días")
	}
	return from.UTC(), to.UTC(), nil
}

func parseCostTime(raw string, inclusiveDateEnd bool) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed.UTC(), nil
	}
	parsed, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return time.Time{}, err
	}
	if inclusiveDateEnd {
		parsed = parsed.AddDate(0, 0, 1)
	}
	return parsed.UTC(), nil
}
