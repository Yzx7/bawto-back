package controllers

import (
	"encoding/json"
	"strings"

	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
	"github.com/gofiber/fiber/v2"
)

func (con *Controller) ListDataObjects(c *fiber.Ctx) error {
	bot, e := con.botWithRole(c, c.Params("botId"))
	if e != nil {
		return con.failErr(c, e)
	}
	v, e := models.ListDataObjects(c.Context(), con.Env.Postgres, bot.ID)
	if e != nil {
		return con.fail(c, 500, "error obteniendo objetos")
	}
	return con.ok(c, "ok", v)
}
func (con *Controller) CreateDataObject(c *fiber.Ctx) error {
	bot, e := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if e != nil {
		return con.failErr(c, e)
	}
	var in struct{ Key, Name, PluralName string }
	if c.BodyParser(&in) != nil || strings.TrimSpace(in.Key) == "" || strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.PluralName) == "" {
		return con.fail(c, 400, "objeto inválido")
	}
	v, e := models.CreateDataObject(c.Context(), con.Env.Postgres, bot.ID, strings.TrimSpace(in.Key), strings.TrimSpace(in.Name), strings.TrimSpace(in.PluralName))
	if e != nil {
		return con.fail(c, 400, "no se pudo crear el objeto")
	}
	return c.Status(201).JSON(types.OK("objeto creado", v))
}
func (con *Controller) ListDataFields(c *fiber.Ctx) error {
	bot, e := con.botWithRole(c, c.Params("botId"))
	if e != nil {
		return con.failErr(c, e)
	}
	v, e := models.ListDataFields(c.Context(), con.Env.Postgres, bot.ID, c.Params("objectId"))
	if e != nil {
		return con.fail(c, 500, "error obteniendo campos")
	}
	return con.ok(c, "ok", v)
}
func (con *Controller) UpsertDataField(c *fiber.Ctx) error {
	bot, e := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if e != nil {
		return con.failErr(c, e)
	}
	var in struct {
		Key, Label, Type string
		Required         bool
	}
	if c.BodyParser(&in) != nil || strings.TrimSpace(in.Key) == "" || strings.TrimSpace(in.Label) == "" {
		return con.fail(c, 400, "campo inválido")
	}
	v, e := models.UpsertDataField(c.Context(), con.Env.Postgres, bot.ID, c.Params("objectId"), strings.TrimSpace(in.Key), strings.TrimSpace(in.Label), strings.TrimSpace(in.Type), in.Required)
	if e != nil {
		return con.fail(c, 400, "no se pudo guardar el campo")
	}
	return c.Status(201).JSON(types.OK("campo guardado", v))
}
func (con *Controller) ListDataRecords(c *fiber.Ctx) error {
	bot, e := con.botWithRole(c, c.Params("botId"))
	if e != nil {
		return con.failErr(c, e)
	}
	v, e := models.ListDataRecords(c.Context(), con.Env.Postgres, bot.ID, c.Params("objectId"))
	if e != nil {
		return con.fail(c, 500, "error obteniendo registros")
	}
	return con.ok(c, "ok", v)
}
func (con *Controller) CreateDataRecord(c *fiber.Ctx) error {
	bot, e := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if e != nil {
		return con.failErr(c, e)
	}
	body := c.Body()
	if !json.Valid(body) {
		return con.fail(c, 400, "registro inválido")
	}
	v, e := models.CreateDataRecord(c.Context(), con.Env.Postgres, bot.ID, c.Params("objectId"), json.RawMessage(body))
	if e != nil {
		return con.fail(c, 400, e.Error())
	}
	return c.Status(201).JSON(types.OK("registro creado", v))
}
func (con *Controller) LinkDataRecordContact(c *fiber.Ctx) error {
	bot, e := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if e != nil {
		return con.failErr(c, e)
	}
	role := strings.TrimSpace(c.Query("role", "primary"))
	if e = models.LinkRecordContact(c.Context(), con.Env.Postgres, bot.ID, c.Params("recordId"), c.Params("contactId"), role); e != nil {
		return con.fail(c, 400, e.Error())
	}
	return con.ok(c, "relación creada", nil)
}
func (con *Controller) ListDataViews(c *fiber.Ctx) error {
	bot, e := con.botWithRole(c, c.Params("botId"))
	if e != nil {
		return con.failErr(c, e)
	}
	v, e := models.ListDataViews(c.Context(), con.Env.Postgres, bot.ID, c.Params("objectId"))
	if e != nil {
		return con.fail(c, 500, "error obteniendo vistas")
	}
	return con.ok(c, "ok", v)
}
func (con *Controller) CreateDataView(c *fiber.Ctx) error {
	bot, e := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if e != nil {
		return con.failErr(c, e)
	}
	var in struct {
		Name   string
		Filter json.RawMessage
	}
	if c.BodyParser(&in) != nil || strings.TrimSpace(in.Name) == "" || (len(in.Filter) > 0 && !json.Valid(in.Filter)) {
		return con.fail(c, 400, "vista inválida")
	}
	v, e := models.CreateDataView(c.Context(), con.Env.Postgres, bot.ID, c.Params("objectId"), strings.TrimSpace(in.Name), in.Filter)
	if e != nil {
		return con.fail(c, 400, "no se pudo crear la vista")
	}
	return c.Status(201).JSON(types.OK("vista creada", v))
}

func (con *Controller) ResolveDataView(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"))
	if err != nil {
		return con.failErr(c, err)
	}
	v, err := models.ResolveDataView(c.Context(), con.Env.Postgres, bot.ID, c.Params("viewId"))
	if err != nil {
		return con.fail(c, 400, err.Error())
	}
	if v == nil {
		return con.fail(c, 404, "vista no encontrada")
	}
	return con.ok(c, "ok", v)
}
