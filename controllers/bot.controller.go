package controllers

import (
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/channels/whatsapp"
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

// GET /bots/:botId/variables — catálogo de variables del bot para el editor.
// Evita que el flujo referencie `{data_factura_x}` cuando el objeto se llama
// `facturas`: el editor marca en rojo lo que no existe aquí.
func (con *Controller) ListFlowVariables(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"))
	if err != nil {
		return con.failErr(c, err)
	}
	vars, err := models.FlowVariables(c.Context(), con.Env.Postgres, bot.ID)
	if err != nil {
		con.Env.Logger.Error("flow variables", "bot", bot.ID, "err", err.Error())
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo leer el catálogo de variables")
	}
	return con.ok(c, "ok", vars)
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

// GET /bots/:botId/channel/health — salud del canal proyectada desde los eventos
// de cuenta de Meta, más el historial reciente.
//
// Va aparte de GET /channel a propósito, aunque respondan sobre lo mismo: aquel
// consulta el número en Meta en vivo y devuelve 502 si el token venció. La salud
// tiene que poder leerse **justo cuando Meta no responde**, que es cuando hace
// falta. Sale entera de Postgres.
func (con *Controller) GetBotChannelHealth(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	if bot.WabaID == nil || *bot.WabaID == "" {
		return con.ok(c, "el bot no tiene WABA asociado", nil)
	}
	health, err := models.GetChannelHealth(c.Context(), con.Env.Postgres, *bot.WabaID)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo leer la salud del canal")
	}
	events, err := models.ListChannelAccountEvents(c.Context(), con.Env.Postgres, *bot.WabaID,
		c.Query("onlyProblems") == "true", c.QueryInt("limit", 50))
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo leer el historial del canal")
	}
	return con.ok(c, "ok", fiber.Map{"health": health, "events": events})
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
		BusinessID    string `json:"businessId"`
		Mode          string `json:"mode"`
		Pin           string `json:"pin"`
	}
	if err := c.BodyParser(&b); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido")
	}
	b.Code = strings.TrimSpace(b.Code)
	b.PhoneNumberID = strings.TrimSpace(b.PhoneNumberID)
	b.WabaID = strings.TrimSpace(b.WabaID)
	b.BusinessID = strings.TrimSpace(b.BusinessID)
	b.Mode = strings.TrimSpace(b.Mode)
	b.Pin = strings.TrimSpace(b.Pin)
	// Solo el code es obligatorio: los ids llegan al panel por postMessage y en un
	// navegador movil no llegan nunca. Si faltan se deducen del token mas abajo,
	// porque descartarlos aqui tira una conexion que en Meta ya quedo hecha.
	if b.Code == "" {
		return con.fail(c, fiber.StatusBadRequest, "code es obligatorio")
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

	// Descubrimiento: el token de ES sabe a que WABA pertenece y que numeros
	// tiene. Solo se consulta lo que el panel no pudo entregar.
	displayPhone := ""
	if b.WabaID == "" {
		wabas, err := whatsapp.DiscoverWABAs(c.Context(), cfg.WhatsAppAPIBase, cfg.WhatsAppAPIVersion,
			cfg.FacebookAppID, cfg.WhatsAppAppSecret, token, nil)
		if err != nil {
			con.Env.Logger.Error("embedded discover wabas", "err", err.Error())
			return con.fail(c, fiber.StatusBadGateway, "no se pudo identificar la cuenta de WhatsApp con Meta")
		}
		switch len(wabas) {
		case 0:
			return con.fail(c, fiber.StatusBadRequest, "el flujo termino sin una cuenta de WhatsApp; reintenta y agrega el numero")
		case 1:
			b.WabaID = wabas[0]
		default:
			// Con varias no se puede adivinar cual quiso el cliente, y elegir mal
			// conectaria el bot al numero equivocado.
			return con.fail(c, fiber.StatusConflict, "el token da acceso a varias cuentas de WhatsApp; repite la conexion desde una computadora")
		}
	}
	if b.PhoneNumberID == "" {
		numbers, err := whatsapp.ListPhoneNumbers(c.Context(), cfg.WhatsAppAPIBase, cfg.WhatsAppAPIVersion, b.WabaID, token, nil)
		if err != nil {
			con.Env.Logger.Error("embedded list phone numbers", "err", err.Error())
			return con.fail(c, fiber.StatusBadGateway, "no se pudieron leer los numeros de la cuenta de WhatsApp")
		}
		switch len(numbers) {
		case 0:
			return con.fail(c, fiber.StatusBadRequest, "la cuenta de WhatsApp no tiene numeros; reintenta y agrega el numero")
		case 1:
			b.PhoneNumberID = numbers[0].ID
			displayPhone = numbers[0].DisplayPhoneNumber
		default:
			return con.fail(c, fiber.StatusConflict, "la cuenta tiene varios numeros; repite la conexion desde una computadora para elegir cual")
		}
	}

	// Dos bots con el mismo phone_number_id rompen el webhook: GetBotByChannel
	// exige exactamente una fila y falla al enrutar el mensaje entrante.
	if owner, err := models.GetBotByChannel(c.Context(), con.Env.Postgres, "wsp", b.PhoneNumberID); err == nil && owner != nil && owner.ID != bot.ID {
		return con.fail(c, fiber.StatusConflict, "ese numero ya esta conectado a otro bot; desconectalo alli antes de usarlo aqui")
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
	if err := models.UpdateBotChannelEmbedded(
		c.Context(), con.Env.Postgres, bot.ID, "wsp", b.PhoneNumberID, displayPhone,
		b.WabaID, b.BusinessID, enc,
	); err != nil {
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
