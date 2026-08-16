package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/channels/whatsapp"
	"github.com/Yzx7/sacs-chatbots/models"
)

func (con *Controller) templateSendConfig(
	ctx context.Context,
	bot *models.Bot,
) (whatsapp.SendConfig, error) {
	if bot == nil || bot.ChannelID == nil || *bot.ChannelID == "" ||
		bot.WabaID == nil || *bot.WabaID == "" {
		return whatsapp.SendConfig{}, fmt.Errorf("el bot no tiene canal y WABA conectados")
	}
	if con.Env.Cipher == nil {
		return whatsapp.SendConfig{}, fmt.Errorf("el cifrado de tokens no está configurado")
	}
	channel, err := models.GetBotByChannel(ctx, con.Env.Postgres, whatsapp.Channel, *bot.ChannelID)
	if err != nil {
		return whatsapp.SendConfig{}, err
	}
	if channel == nil || len(channel.TokenEnc) == 0 {
		return whatsapp.SendConfig{}, fmt.Errorf("el canal no tiene token guardado")
	}
	token, err := con.Env.Cipher.Decrypt(channel.TokenEnc)
	if err != nil {
		return whatsapp.SendConfig{}, fmt.Errorf("no se pudo descifrar el token")
	}
	cfg := con.Env.Config
	return whatsapp.SendConfig{
		APIBase: cfg.WhatsAppAPIBase, Version: cfg.WhatsAppAPIVersion,
		PhoneNumberID: *bot.ChannelID, Token: token,
	}, nil
}

func (con *Controller) syncBotTemplates(
	ctx context.Context,
	bot *models.Bot,
) (*models.TemplateSyncReport, error) {
	sendCfg, err := con.templateSendConfig(ctx, bot)
	if err != nil {
		return nil, err
	}
	templates, err := whatsapp.ListTemplates(ctx, sendCfg, *bot.WabaID)
	if err != nil {
		return nil, err
	}
	return models.SyncChannelTemplates(ctx, con.Env.Postgres, *bot.WabaID, templates, time.Now().UTC())
}

// GET /bots/:botId/templates — catálogo local; no llama a Meta ni oculta
// plantillas eliminadas porque pueden seguir referenciadas por versiones viejas.
func (con *Controller) ListBotTemplates(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"))
	if err != nil {
		return con.failErr(c, err)
	}
	templates, err := models.ListChannelTemplatesForBot(c.Context(), con.Env.Postgres, bot.ID)
	if err != nil {
		con.Env.Logger.Error("list templates", "bot", bot.ID, "err", err.Error())
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo leer el catálogo de plantillas")
	}
	return con.ok(c, "ok", templates)
}

// POST /bots/:botId/templates/sync — fotografía completa y paginada del WABA.
func (con *Controller) SyncBotTemplates(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	report, err := con.syncBotTemplates(c.Context(), bot)
	if err != nil {
		con.Env.Logger.Error("sync templates", "bot", bot.ID, "err", err.Error())
		return con.fail(c, fiber.StatusBadGateway, "no se pudo sincronizar el catálogo con Meta: "+err.Error())
	}
	return con.ok(c, "plantillas sincronizadas", report)
}

// POST /bots/:botId/templates/:name/test — envío controlado a un contacto que
// ya pertenece a la organización. No acepta un teléfono arbitrario.
func (con *Controller) TestBotTemplate(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	var input struct {
		ContactID string   `json:"contactId"`
		Language  string   `json:"language"`
		Params    []string `json:"params"`
	}
	if c.BodyParser(&input) != nil || strings.TrimSpace(input.ContactID) == "" ||
		strings.TrimSpace(input.Language) == "" {
		return con.fail(c, fiber.StatusBadRequest, "contactId e idioma son obligatorios")
	}
	if _, err = con.syncBotTemplates(c.Context(), bot); err != nil {
		return con.fail(c, fiber.StatusBadGateway, "no se pudo confirmar la plantilla con Meta")
	}
	name := strings.TrimSpace(c.Params("name"))
	language := strings.TrimSpace(input.Language)
	template, err := models.GetChannelTemplateForBot(c.Context(), con.Env.Postgres, bot.ID, name, language)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo leer la plantilla")
	}
	if template == nil || !strings.EqualFold(template.Status, "APPROVED") {
		return con.fail(c, fiber.StatusConflict, "la plantilla no existe o no está APPROVED")
	}
	if template.HasUnsupportedParameters || len(input.Params) != template.BodyParameterCount {
		return con.fail(c, fiber.StatusBadRequest,
			fmt.Sprintf("la plantilla requiere %d parámetros BODY compatibles", template.BodyParameterCount))
	}
	contact, err := models.GetContact(c.Context(), con.Env.Postgres, bot.ID, input.ContactID)
	if err != nil || contact == nil {
		return con.fail(c, fiber.StatusNotFound, "contacto no encontrado")
	}
	if contact.Status != "active" {
		return con.fail(c, fiber.StatusConflict, "el contacto no está activo")
	}
	sendCfg, err := con.templateSendConfig(c.Context(), bot)
	if err != nil {
		return con.fail(c, fiber.StatusFailedDependency, err.Error())
	}
	destino := whatsapp.Recipient{Phone: contact.PhoneNormalized, UserID: contact.ChannelUserID}
	providerID, err := whatsapp.SendTemplate(c.Context(), sendCfg, destino, name, language, input.Params)
	if err != nil {
		return con.fail(c, fiber.StatusBadGateway, "Meta rechazó el envío: "+err.Error())
	}
	chatID, err := models.UpsertChat(c.Context(), con.Env.Postgres, bot.ID, contact.ID)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "Meta aceptó el mensaje, pero no se pudo crear el chat")
	}
	metadata, _ := json.Marshal(fiber.Map{
		"templateName": name, "templateLanguage": language, "controlledTest": true,
	})
	body := "[Plantilla " + name + "]"
	if len(input.Params) > 0 {
		body += " " + strings.Join(input.Params, " · ")
	}
	message, err := models.InsertOutboundMessageWithMetadata(
		c.Context(), con.Env.Postgres, chatID, providerID, "template", body, metadata)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "Meta aceptó el mensaje, pero no se pudo persistir")
	}
	if message != nil {
		con.publishChat(c.Context(), "message", bot.ID, chatID, message)
	}
	con.reconcileStatusEvents(c.Context(), providerID)
	return con.ok(c, "plantilla enviada", fiber.Map{
		"providerMessageId": providerID, "chatId": chatID, "message": message,
	})
}
