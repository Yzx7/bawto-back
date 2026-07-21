package controllers

import (
	"encoding/json"
	"strings"

	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
	"github.com/gofiber/fiber/v2"
)

func (con *Controller) ListContactFields(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"))
	if err != nil {
		return con.failErr(c, err)
	}
	v, err := models.ListContactFields(c.Context(), con.Env.Postgres, bot.ID)
	if err != nil {
		return con.fail(c, 500, "error obteniendo campos")
	}
	return con.ok(c, "ok", v)
}

func (con *Controller) UpsertContactField(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	var in struct {
		Key, Label, Type string
		Required         bool
	}
	if err := c.BodyParser(&in); err != nil || strings.TrimSpace(in.Key) == "" || strings.TrimSpace(in.Label) == "" {
		return con.fail(c, 400, "campo inválido")
	}
	v, err := models.UpsertContactField(c.Context(), con.Env.Postgres, bot.ID, strings.TrimSpace(in.Key), strings.TrimSpace(in.Label), strings.TrimSpace(in.Type), in.Required)
	if err != nil {
		return con.fail(c, 400, "no se pudo guardar el campo")
	}
	return c.Status(fiber.StatusCreated).JSON(types.OK("campo guardado", v))
}

func (con *Controller) ListAudiences(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"))
	if err != nil {
		return con.failErr(c, err)
	}
	v, err := models.ListAudiences(c.Context(), con.Env.Postgres, bot.ID)
	if err != nil {
		return con.fail(c, 500, "error obteniendo audiencias")
	}
	return con.ok(c, "ok", v)
}

func (con *Controller) CreateAudience(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	var in struct {
		Name, Kind string
		Filter     json.RawMessage
	}
	if err := c.BodyParser(&in); err != nil || strings.TrimSpace(in.Name) == "" || (len(in.Filter) > 0 && !json.Valid(in.Filter)) {
		return con.fail(c, 400, "audiencia inválida")
	}
	if in.Kind == "" {
		in.Kind = "dynamic"
	}
	v, err := models.CreateAudience(c.Context(), con.Env.Postgres, bot.ID, strings.TrimSpace(in.Name), strings.TrimSpace(in.Kind), in.Filter)
	if err != nil {
		return con.fail(c, 400, "no se pudo crear la audiencia")
	}
	return c.Status(fiber.StatusCreated).JSON(types.OK("audiencia creada", v))
}

func (con *Controller) AddAudienceContact(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	if err := models.AddAudienceContact(c.Context(), con.Env.Postgres, bot.ID, c.Params("audienceId"), c.Params("contactId")); err != nil {
		return con.fail(c, 400, err.Error())
	}
	return con.ok(c, "contacto agregado", nil)
}

func (con *Controller) ResolveAudience(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"))
	if err != nil {
		return con.failErr(c, err)
	}
	v, err := models.ResolveAudience(c.Context(), con.Env.Postgres, bot.ID, c.Params("audienceId"))
	if err != nil {
		return con.fail(c, 400, "no se pudo resolver la audiencia")
	}
	return con.ok(c, "ok", v)
}
