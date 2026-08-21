package controllers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"

	"github.com/Yzx7/sacs-chatbots/channels"
	"github.com/Yzx7/sacs-chatbots/channels/whatsapp"
	"github.com/Yzx7/sacs-chatbots/events"
	"github.com/Yzx7/sacs-chatbots/models"
)

// Bandeja de atención humana. La autorización siempre sale de la organización
// dueña del bot: el chat solo se resuelve tras validar la membresía.

// chatWithRole carga el chat y exige rol en la org dueña del bot.
func (con *Controller) chatWithRole(c *fiber.Ctx, roles ...string) (*models.ChatMeta, error) {
	meta, err := models.GetChatMeta(c.Context(), con.Env.Postgres, c.Params("chatId"))
	if err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "chat inválido")
	}
	if meta == nil {
		return nil, fiber.NewError(fiber.StatusNotFound, "chat no encontrado")
	}
	if _, err := con.requireOrgRole(c, meta.OrgID, roles...); err != nil {
		return nil, err
	}
	return meta, nil
}

// GET /bots/:botId/chats — conversaciones del bot (cursor `before`, búsqueda `q`).
func (con *Controller) ListBotChats(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"))
	if err != nil {
		return con.failErr(c, err)
	}
	var before *time.Time
	if raw := c.Query("before"); raw != "" {
		if t, e := time.Parse(time.RFC3339Nano, raw); e == nil {
			before = &t
		}
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	mode := strings.TrimSpace(c.Query("mode"))
	if mode != "" && mode != "bot" && mode != "manual" {
		return con.fail(c, fiber.StatusBadRequest, `mode debe ser "bot" o "manual"`)
	}
	chats, err := models.ListChats(c.Context(), con.Env.Postgres, bot.ID, strings.TrimSpace(c.Query("q")), mode, before, limit)
	if err != nil {
		con.Env.Logger.Error("list chats", "bot", bot.ID, "err", err.Error())
		return con.fail(c, fiber.StatusInternalServerError, "no se pudieron listar los chats")
	}
	return con.ok(c, "ok", chats)
}

// GET /chats/:chatId — datos del chat (incluye si la ventana de 24 h sigue abierta).
func (con *Controller) GetChat(c *fiber.Ctx) error {
	meta, err := con.chatWithRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	return con.ok(c, "ok", chatView(meta))
}

// GET /chats/:chatId/messages — historial paginado hacia atrás (`before` = id).
func (con *Controller) ListChatMessages(c *fiber.Ctx) error {
	meta, err := con.chatWithRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	before, _ := strconv.ParseInt(c.Query("before"), 10, 64)
	limit, _ := strconv.Atoi(c.Query("limit"))
	msgs, err := models.ListMessages(c.Context(), con.Env.Postgres, meta.ID, before, limit)
	if err != nil {
		con.Env.Logger.Error("list messages", "chat", meta.ID, "err", err.Error())
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo leer el historial")
	}
	return con.ok(c, "ok", msgs)
}

// POST /chats/:chatId/messages — responde a mano desde el panel.
//
// Enviar manualmente **toma la conversación**: el chat pasa a modo manual para
// que el bot no pise la respuesta del agente (clave con Coexistence).
func (con *Controller) SendChatMessage(c *fiber.Ctx) error {
	meta, err := con.chatWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	var b struct {
		Text string `json:"text"`
	}
	if err := c.BodyParser(&b); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido")
	}
	b.Text = strings.TrimSpace(b.Text)
	if b.Text == "" {
		return con.fail(c, fiber.StatusBadRequest, "el mensaje está vacío")
	}
	// Fuera de la ventana de servicio Meta solo acepta plantillas aprobadas.
	if !meta.WindowOpen() {
		return con.fail(c, fiber.StatusFailedDependency,
			"pasaron más de 24 h desde el último mensaje del cliente: WhatsApp solo permite plantillas aprobadas")
	}

	cfg, err := con.sendConfigFor(c.Context(), meta.BotID)
	if err != nil {
		return con.failErr(c, err)
	}
	destino := whatsapp.Recipient{Phone: meta.ContactPhone, UserID: meta.ContactUserID}
	waID, err := whatsapp.SendText(c.Context(), cfg, destino, b.Text)
	if err != nil {
		con.whatsAppLogger().Error("envío manual", "chat", meta.ID, "err", err.Error())
		return con.fail(c, fiber.StatusBadGateway, "WhatsApp rechazó el envío: "+err.Error())
	}

	msg, err := models.InsertOutboundMessage(c.Context(), con.Env.Postgres, meta.ID, waID, "text", b.Text)
	if err != nil {
		// El mensaje ya salió: no es un error para el agente, pero hay que verlo.
		con.Env.Logger.Error("guardar envío manual", "chat", meta.ID, "err", err.Error())
		return con.ok(c, "enviado (no se pudo guardar en el historial)", nil)
	}
	if err := con.takeOver(c.Context(), meta); err != nil {
		con.Env.Logger.Error("handoff por envío manual", "chat", meta.ID, "err", err.Error())
	}
	con.publishChat(c.Context(), "message", meta.BotID, meta.ID, msg)
	con.reconcileStatusEvents(c.Context(), waID)
	return con.ok(c, "enviado", msg)
}

// POST /chats/:chatId/reset — olvida en qué punto del flujo quedó la conversación.
//
// El estado (`chats.current_layer`) solo lo escribía el webhook, así que una
// conversación abandonada a medias se quedaba ahí para siempre. Eso no es
// cosmético: `ReminderChatBlock` la trata como conversación activa y **pospone
// los recordatorios** de ese contacto —dos horas, tres veces, y luego los
// cancela—. Sin esta acción, el propio cliente se bloqueaba sus avisos y no había
// forma de destrabarlo desde el panel.
//
// No toca el modo: si el chat está en atención manual, sigue estándolo. Son dos
// decisiones distintas y mezclarlas haría que reiniciar el flujo devolviera el
// chat al bot sin que nadie lo pidiera.
func (con *Controller) ResetChatFlowState(c *fiber.Ctx) error {
	meta, err := con.chatWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	if err := models.SetChatState(c.Context(), con.Env.Postgres, meta.ID, json.RawMessage("null")); err != nil {
		con.Env.Logger.Error("reset estado de chat", "chat", meta.ID, "err", err.Error())
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo reiniciar la conversación")
	}
	fresh, _ := models.GetChatMeta(c.Context(), con.Env.Postgres, meta.ID)
	if fresh == nil {
		fresh = meta
	}
	con.publishChat(c.Context(), "mode", fresh.BotID, fresh.ID, chatModeView(fresh))
	return con.ok(c, "conversación reiniciada", chatView(fresh))
}

// PUT /chats/:chatId/mode — alterna quién atiende: el bot o una persona.
// `hours` (opcional) limita el handoff; 0 = indefinido.
func (con *Controller) SetChatMode(c *fiber.Ctx) error {
	meta, err := con.chatWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	var b struct {
		Mode  string `json:"mode"`
		Hours int    `json:"hours"`
	}
	if err := c.BodyParser(&b); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido")
	}
	switch b.Mode {
	case "bot":
		err = models.SetChatMode(c.Context(), con.Env.Postgres, meta.ID, "bot", nil)
	case "manual":
		err = models.HandoffChat(c.Context(), con.Env.Postgres, meta.ID, time.Duration(b.Hours)*time.Hour)
	default:
		return con.fail(c, fiber.StatusBadRequest, `mode debe ser "bot" o "manual"`)
	}
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo cambiar el modo")
	}
	fresh, _ := models.GetChatMeta(c.Context(), con.Env.Postgres, meta.ID)
	if fresh == nil {
		fresh = meta
	}
	con.publishChat(c.Context(), "mode", fresh.BotID, fresh.ID, chatModeView(fresh))
	return con.ok(c, "modo actualizado", chatView(fresh))
}

// POST /chats/:chatId/read — mueve el corte de no leídos.
func (con *Controller) MarkChatRead(c *fiber.Ctx) error {
	meta, err := con.chatWithRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	if err := models.MarkChatRead(c.Context(), con.Env.Postgres, meta.ID); err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo marcar como leído")
	}
	return con.ok(c, "ok", nil)
}

// GET /messages/:messageId/media — sirve la copia local del archivo.
func (con *Controller) GetMessageMedia(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("messageId"), 10, 64)
	if err != nil {
		return con.fail(c, fiber.StatusBadRequest, "id inválido")
	}
	md, err := models.GetMessageMedia(c.Context(), con.Env.Postgres, id)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo leer el archivo")
	}
	if md == nil {
		return con.fail(c, fiber.StatusNotFound, "archivo no encontrado")
	}
	if _, err := con.requireOrgRole(c, md.OrgID); err != nil {
		return con.failErr(c, err)
	}
	c.Set("Content-Type", md.MimeType)
	c.Set("Cache-Control", "private, max-age=86400")
	return c.Send(md.Data)
}

// GET /bots/:botId/stream — SSE con los eventos en vivo del bot.
func (con *Controller) StreamBotEvents(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"))
	if err != nil {
		return con.failErr(c, err)
	}
	if con.Env.Events == nil {
		return con.fail(c, fiber.StatusServiceUnavailable, "eventos en vivo no disponibles")
	}
	sub, unsub := con.Env.Events.Subscribe(bot.ID)

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no") // nginx no debe bufferizar el stream

	c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
		defer unsub()
		// El único modo de detectar que el cliente se fue es que falle el Flush.
		write := func(s string) bool {
			if _, err := w.WriteString(s); err != nil {
				return false
			}
			return w.Flush() == nil
		}
		if !write(": conectado\n\n") {
			return
		}
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		for {
			select {
			case e, open := <-sub:
				if !open {
					return
				}
				raw, err := json.Marshal(e)
				if err != nil {
					continue
				}
				if !write(fmt.Sprintf("event: %s\ndata: %s\n\n", e.Type, raw)) {
					return
				}
			case <-ping.C:
				if !write(": ping\n\n") { // comentario SSE: mantiene viva la conexión
					return
				}
			}
		}
	}))
	return nil
}

// ---- helpers ----

// chatView expone el chat al panel añadiendo el estado de la ventana de 24 h.
func chatView(m *models.ChatMeta) fiber.Map {
	return fiber.Map{
		"id":               m.ID,
		"botId":            m.BotID,
		"contactId":        m.ContactID,
		"contact":          m.Contact,
		"contactPhone":     m.ContactPhone,
		"contactUserId":    m.ContactUserID,
		"username":         m.Username,
		"contactName":      m.ContactName,
		"contactData":      m.ContactData,
		"contactStatus":    m.ContactStatus,
		"contactCreatedAt": m.ContactCreatedAt,
		"contactUpdatedAt": m.ContactUpdatedAt,
		"mode":             m.Mode,
		"handoffUntil":     m.HandoffUntil,
		"lastInboundAt":    m.LastInboundAt,
		"windowOpen":       m.WindowOpen(),
	}
}

// chatModeView mantiene pequeño el NOTIFY. Los campos CRM pueden superar el
// límite de PostgreSQL y convertir el evento de modo en un payload vacío.
func chatModeView(m *models.ChatMeta) fiber.Map {
	return fiber.Map{
		"id":            m.ID,
		"botId":         m.BotID,
		"contact":       m.Contact,
		"contactName":   m.ContactName,
		"mode":          m.Mode,
		"handoffUntil":  m.HandoffUntil,
		"lastInboundAt": m.LastInboundAt,
		"windowOpen":    m.WindowOpen(),
	}
}

// takeOver deja la conversación en manos del agente humano.
func (con *Controller) takeOver(ctx context.Context, meta *models.ChatMeta) error {
	if meta.Mode == "manual" {
		return nil
	}
	meta.Mode = "manual"
	return models.HandoffChat(ctx, con.Env.Postgres, meta.ID, handoffWindow)
}

// publishChat emite un evento de chat (no crítico: si falla, el panel refresca solo).
func (con *Controller) publishChat(ctx context.Context, kind, botID, chatID string, payload any) {
	if con.Env.Events == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	con.Env.Events.Publish(ctx, events.Event{Type: kind, BotID: botID, ChatID: chatID, Payload: raw})
}

// publishMode anuncia quién atiende el chat tras un cambio de modo.
func (con *Controller) publishMode(ctx context.Context, botID, chatID string) {
	meta, err := models.GetChatMeta(ctx, con.Env.Postgres, chatID)
	if err != nil || meta == nil {
		return
	}
	con.publishChat(ctx, "mode", botID, chatID, chatModeView(meta))
}

// publishInbound anuncia un mensaje entrante recién guardado por el webhook.
func (con *Controller) publishInbound(ctx context.Context, botID, chatID string, messageID int64, m channels.InboundMessage, hasMedia bool) {
	con.publishChat(ctx, "message", botID, chatID, models.Message{
		ID:        messageID,
		WaID:      &m.WaID,
		FromMe:    false,
		Type:      string(m.Type),
		Body:      &m.Text,
		HasMedia:  hasMedia,
		CreatedAt: time.Now(),
	})
}

// sendConfigFor arma la configuración de envío del bot (descifra su token).
func (con *Controller) sendConfigFor(ctx context.Context, botID string) (whatsapp.SendConfig, error) {
	var out whatsapp.SendConfig
	bot, err := models.GetBot(ctx, con.Env.Postgres, botID)
	if err != nil || bot == nil {
		return out, fiber.NewError(fiber.StatusInternalServerError, "no se pudo leer el bot")
	}
	if bot.ChannelID == nil || *bot.ChannelID == "" {
		return out, fiber.NewError(fiber.StatusFailedDependency, "el bot no tiene número de WhatsApp conectado")
	}
	ch, err := models.GetBotByChannel(ctx, con.Env.Postgres, whatsapp.Channel, *bot.ChannelID)
	if err != nil || ch == nil {
		return out, fiber.NewError(fiber.StatusInternalServerError, "no se pudo leer el canal")
	}
	if con.Env.Cipher == nil || len(ch.TokenEnc) == 0 {
		return out, fiber.NewError(fiber.StatusFailedDependency, "el canal no tiene token guardado")
	}
	token, err := con.Env.Cipher.Decrypt(ch.TokenEnc)
	if err != nil {
		return out, fiber.NewError(fiber.StatusInternalServerError, "no se pudo descifrar el token")
	}
	cfg := con.Env.Config
	return whatsapp.SendConfig{
		APIBase:       cfg.WhatsAppAPIBase,
		Version:       cfg.WhatsAppAPIVersion,
		PhoneNumberID: *bot.ChannelID,
		Token:         token,
	}, nil
}
