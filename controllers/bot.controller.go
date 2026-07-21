package controllers

import (
	"encoding/json"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/channels/whatsapp"
	"github.com/Yzx7/sacs-chatbots/engine"
	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
)

// botWithRole carga el bot y exige que el usuario tenga uno de `roles` en la org
// dueña del bot. Reutiliza requireOrgRole. Devuelve el bot o un *fiber.Error.
func (con *Controller) botWithRole(c *fiber.Ctx, botID string, roles ...string) (*models.Bot, error) {
	bot, err := models.GetBot(c.Context(), con.Env.Postgres, botID)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, "error obteniendo el bot")
	}
	if bot == nil {
		return nil, fiber.NewError(fiber.StatusNotFound, "bot no encontrado")
	}
	if _, err := con.requireOrgRole(c, bot.OrgID, roles...); err != nil {
		return nil, err
	}
	return bot, nil
}

// GET /orgs/:orgId/bots — bots de la organización (cualquier miembro).
func (con *Controller) GetOrgBots(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID); err != nil {
		return con.failErr(c, err)
	}
	bots, err := models.ListBotsByOrg(c.Context(), con.Env.Postgres, orgID)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "error obteniendo bots")
	}
	return con.ok(c, "ok", bots)
}

// POST /orgs/:orgId/bots — crea un bot (owner/admin).
func (con *Controller) CreateBot(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID, "owner", "admin"); err != nil {
		return con.failErr(c, err)
	}
	var b struct {
		Name    string `json:"name"`
		Channel string `json:"channel"`
	}
	if err := c.BodyParser(&b); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido")
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" {
		return con.fail(c, fiber.StatusBadRequest, "el nombre es obligatorio")
	}
	bot, err := models.CreateBot(c.Context(), con.Env.Postgres, orgID, b.Name, b.Channel)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo crear el bot")
	}
	return c.Status(fiber.StatusCreated).JSON(types.OK("bot creado", bot))
}

// GET /bots/:botId — detalle del bot (cualquier miembro).
func (con *Controller) GetBot(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"))
	if err != nil {
		return con.failErr(c, err)
	}
	return con.ok(c, "ok", bot)
}

// PUT /bots/:botId — renombra (owner/admin/member).
func (con *Controller) UpdateBot(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	var b struct {
		Name string `json:"name"`
	}
	if err := c.BodyParser(&b); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido")
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" {
		return con.fail(c, fiber.StatusBadRequest, "el nombre es obligatorio")
	}
	if err := models.UpdateBotName(c.Context(), con.Env.Postgres, bot.ID, b.Name); err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo actualizar")
	}
	return con.ok(c, "bot actualizado", nil)
}

// DELETE /bots/:botId — elimina (owner/admin).
func (con *Controller) DeleteBot(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin")
	if err != nil {
		return con.failErr(c, err)
	}
	if err := models.DeleteBot(c.Context(), con.Env.Postgres, bot.ID); err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo eliminar")
	}
	return con.ok(c, "bot eliminado", nil)
}

// GET /bots/:botId/flow — grafo del flujo (cualquier miembro).
func (con *Controller) GetBotFlow(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"))
	if err != nil {
		return con.failErr(c, err)
	}
	return con.ok(c, "ok", bot.Flow)
}

// PUT /bots/:botId/flow — guarda el grafo del flujo (owner/admin/member).
func (con *Controller) UpdateBotFlow(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	body := c.Body()
	if !json.Valid(body) {
		return con.fail(c, fiber.StatusBadRequest, "flujo inválido (no es JSON)")
	}
	var flow engine.Flow
	if err := json.Unmarshal(body, &flow); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "flujo inválido")
	}
	if err := engine.Validate(&flow); err != nil {
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}
	if err := models.UpdateBotFlow(c.Context(), con.Env.Postgres, bot.ID, json.RawMessage(body)); err != nil {
		con.Env.Logger.Error("UpdateBotFlow", "botId", bot.ID, "err", err.Error())
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo guardar el flujo")
	}
	return con.ok(c, "flujo guardado", nil)
}

// PUT /bots/:botId/channel — conecta el bot a WhatsApp (owner/admin).
func (con *Controller) ConnectBotChannel(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin")
	if err != nil {
		return con.failErr(c, err)
	}
	if con.Env.Cipher == nil {
		return con.fail(c, fiber.StatusInternalServerError, "cifrado no configurado (TOKEN_ENC_KEY)")
	}
	var b struct {
		PhoneNumberID string `json:"phoneNumberId"`
		Phone         string `json:"phone"`
		Token         string `json:"token"`
	}
	if err := c.BodyParser(&b); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido")
	}
	b.PhoneNumberID = strings.TrimSpace(b.PhoneNumberID)
	b.Token = strings.TrimSpace(b.Token)
	if b.PhoneNumberID == "" || b.Token == "" {
		return con.fail(c, fiber.StatusBadRequest, "phoneNumberId y token son obligatorios")
	}
	enc, err := con.Env.Cipher.Encrypt(b.Token)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo cifrar el token")
	}
	if err := models.UpdateBotChannel(c.Context(), con.Env.Postgres, bot.ID, "wsp", b.PhoneNumberID, b.Phone, enc); err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo conectar el canal (¿phone_number_id ya usado?)")
	}
	return con.ok(c, "canal conectado", nil)
}

// GET /bots/:botId/channel — info del número conectado (consulta a la Cloud API).
// Devuelve data=null si el bot aún no tiene canal.
func (con *Controller) GetBotChannel(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	if bot.ChannelID == nil || *bot.ChannelID == "" {
		return con.ok(c, "sin canal conectado", nil)
	}

	ch, err := models.GetBotByChannel(c.Context(), con.Env.Postgres, whatsapp.Channel, *bot.ChannelID)
	if err != nil || ch == nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo leer el canal")
	}
	if con.Env.Cipher == nil || len(ch.TokenEnc) == 0 {
		return con.fail(c, fiber.StatusFailedDependency, "el canal no tiene token guardado")
	}
	token, err := con.Env.Cipher.Decrypt(ch.TokenEnc)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo descifrar el token")
	}

	cfg := con.Env.Config
	info, err := whatsapp.GetPhoneInfo(c.Context(), whatsapp.SendConfig{
		APIBase:       cfg.WhatsAppAPIBase,
		Version:       cfg.WhatsAppAPIVersion,
		PhoneNumberID: *bot.ChannelID,
		Token:         token,
	})
	if err != nil {
		con.Env.Logger.Error("wa phone info", "botId", bot.ID, "err", err.Error())
		return con.fail(c, fiber.StatusBadGateway, "no se pudo consultar el número en Meta (¿token vencido?)")
	}
	return con.ok(c, "ok", info)
}

// POST /bots/:botId/channel/embedded conecta el canal via Embedded Signup de Meta.
// Cloud API registra el numero; Coexistence conserva el registro de la app movil.
func (con *Controller) ConnectBotChannelEmbedded(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin")
	if err != nil {
		return con.failErr(c, err)
	}
	if con.Env.Cipher == nil {
		return con.fail(c, fiber.StatusInternalServerError, "cifrado no configurado (TOKEN_ENC_KEY)")
	}
	cfg := con.Env.Config
	if cfg.FacebookAppID == "" || cfg.WhatsAppAppSecret == "" {
		return con.fail(c, fiber.StatusInternalServerError, "FACEBOOK_APP_ID / WHATSAPP_APP_SECRET no configurados")
	}

	var b struct {
		Code          string `json:"code"`
		PhoneNumberID string `json:"phoneNumberId"`
		WabaID        string `json:"wabaId"`
		Mode          string `json:"mode"`
		Pin           string `json:"pin"`
	}
	if err := c.BodyParser(&b); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido")
	}
	b.Code = strings.TrimSpace(b.Code)
	b.PhoneNumberID = strings.TrimSpace(b.PhoneNumberID)
	b.WabaID = strings.TrimSpace(b.WabaID)
	b.Mode = strings.TrimSpace(b.Mode)
	b.Pin = strings.TrimSpace(b.Pin)
	if b.Code == "" || b.PhoneNumberID == "" || b.WabaID == "" {
		return con.fail(c, fiber.StatusBadRequest, "code, phoneNumberId y wabaId son obligatorios")
	}
	if b.Mode != "cloud" && b.Mode != "coexistence" {
		return con.fail(c, fiber.StatusBadRequest, "modo de conexion invalido")
	}
	if b.Mode == "cloud" && (len(b.Pin) != 6 || strings.Trim(b.Pin, "0123456789") != "") {
		return con.fail(c, fiber.StatusBadRequest, "el PIN debe tener 6 digitos")
	}

	token, err := whatsapp.ExchangeCode(c.Context(), cfg.WhatsAppAPIBase, cfg.WhatsAppAPIVersion,
		cfg.FacebookAppID, cfg.WhatsAppAppSecret, b.Code, nil)
	if err != nil {
		con.Env.Logger.Error("embedded exchange", "err", err.Error())
		return con.fail(c, fiber.StatusBadGateway, "no se pudo intercambiar el código con Meta")
	}

	if b.Mode == "cloud" {
		if err := whatsapp.RegisterPhone(c.Context(), cfg.WhatsAppAPIBase, cfg.WhatsAppAPIVersion, b.PhoneNumberID, token, b.Pin, nil); err != nil {
			con.Env.Logger.Error("embedded register phone", "err", err.Error())
			return con.fail(c, fiber.StatusBadGateway, "Meta no pudo registrar el numero; revisa el PIN y los permisos")
		}
	}

	if err := whatsapp.SubscribeWABA(c.Context(), cfg.WhatsAppAPIBase, cfg.WhatsAppAPIVersion, b.WabaID, token, nil); err != nil {
		con.Env.Logger.Error("embedded subscribe waba", "err", err.Error())
		return con.fail(c, fiber.StatusBadGateway, "Meta registro el numero, pero no pudo activar los webhooks")
	}

	enc, err := con.Env.Cipher.Encrypt(token)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo cifrar el token")
	}
	if err := models.UpdateBotChannel(c.Context(), con.Env.Postgres, bot.ID, "wsp", b.PhoneNumberID, "", enc); err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo conectar el canal (¿phone_number_id ya usado?)")
	}
	return con.ok(c, "canal conectado por Embedded Signup", nil)
}

// POST /bots/:botId/channel/register completa el registro de un canal Cloud API
// que ya tiene token guardado, sin repetir Embedded Signup.
func (con *Controller) RegisterBotChannel(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin")
	if err != nil {
		return con.failErr(c, err)
	}
	if bot.ChannelID == nil || *bot.ChannelID == "" || con.Env.Cipher == nil {
		return con.fail(c, fiber.StatusFailedDependency, "el bot no tiene un canal con token guardado")
	}
	channel, err := models.GetBotByChannel(c.Context(), con.Env.Postgres, whatsapp.Channel, *bot.ChannelID)
	if err != nil || channel == nil || len(channel.TokenEnc) == 0 {
		return con.fail(c, fiber.StatusFailedDependency, "el bot no tiene un canal con token guardado")
	}
	var b struct {
		Pin string `json:"pin"`
	}
	if err := c.BodyParser(&b); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input invalido")
	}
	b.Pin = strings.TrimSpace(b.Pin)
	if len(b.Pin) != 6 || strings.Trim(b.Pin, "0123456789") != "" {
		return con.fail(c, fiber.StatusBadRequest, "el PIN debe tener 6 digitos")
	}
	token, err := con.Env.Cipher.Decrypt(channel.TokenEnc)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo descifrar el token")
	}
	cfg := con.Env.Config
	if err := whatsapp.RegisterPhone(c.Context(), cfg.WhatsAppAPIBase, cfg.WhatsAppAPIVersion, *bot.ChannelID, token, b.Pin, nil); err != nil {
		con.Env.Logger.Error("register existing phone", "botId", bot.ID, "err", err.Error())
		return con.fail(c, fiber.StatusBadGateway, "Meta no pudo registrar el numero; revisa el PIN y los permisos")
	}
	return con.ok(c, "numero registrado en Cloud API", nil)
}
