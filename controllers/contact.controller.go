package controllers

import (
	"strings"

	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
	"github.com/gofiber/fiber/v2"
)

// Facturación de un contacto. El CRUD y la importación de contactos viven en
// `organization_data.controller.go`: un contacto es de la organización, no del
// bot que le escribió.
func (con *Controller) ListBilling(c *fiber.Ctx) error {
	if _, err := con.botWithRole(c, c.Params("botId")); err != nil {
		return con.failErr(c, err)
	}
	billing, err := models.ListBillingByContact(c.Context(), con.Env.Postgres, c.Params("botId"), c.Params("contactId"))
	if err != nil {
		return con.fail(c, 500, "error obteniendo cobros")
	}
	return con.ok(c, "ok", billing)
}

func (con *Controller) CreateBilling(c *fiber.Ctx) error {
	if _, err := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member"); err != nil {
		return con.failErr(c, err)
	}
	var in struct{ Period, Amount, Currency, DueDate string }
	if err := c.BodyParser(&in); err != nil || strings.TrimSpace(in.Period) == "" || strings.TrimSpace(in.Amount) == "" || strings.TrimSpace(in.DueDate) == "" {
		return con.fail(c, 400, "cobro inválido")
	}
	billing, err := models.CreateBillingRecord(c.Context(), con.Env.Postgres, c.Params("botId"), c.Params("contactId"), strings.TrimSpace(in.Period), strings.TrimSpace(in.Amount), strings.TrimSpace(in.Currency), strings.TrimSpace(in.DueDate))
	if err != nil {
		return con.fail(c, 400, "no se pudo crear el cobro")
	}
	return c.Status(fiber.StatusCreated).JSON(types.OK("cobro creado", billing))
}
