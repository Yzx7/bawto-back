package controllers

import (
	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/models"
)

// GET /orgs/:orgId/billing — estimado vigente, uso técnico y documentos.
// Es solo lectura: las tarifas las administra Bawto mediante su CLI interna.
func (con *Controller) GetOrganizationBilling(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID); err != nil {
		return con.failErr(c, err)
	}
	from, to, err := costPeriod(c.Query("from"), c.Query("to"), c.Context().Time())
	if err != nil {
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}
	overview, err := models.GetOrganizationBillingOverview(c.Context(), con.Env.Postgres, orgID, from, to)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo calcular la facturación")
	}
	return con.ok(c, "facturación calculada", overview)
}

// GET /orgs/:orgId/billing/statements/:statementId — snapshot emitido.
func (con *Controller) GetOrganizationBillingStatement(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID); err != nil {
		return con.failErr(c, err)
	}
	statement, err := models.GetBillingStatement(c.Context(), con.Env.Postgres, orgID, c.Params("statementId"))
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo obtener el estado de cuenta")
	}
	if statement == nil {
		return con.fail(c, fiber.StatusNotFound, "estado de cuenta no encontrado")
	}
	return con.ok(c, "ok", statement)
}
