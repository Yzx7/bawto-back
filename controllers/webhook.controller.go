package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/channels"
	"github.com/Yzx7/sacs-chatbots/channels/whatsapp"
	"github.com/Yzx7/sacs-chatbots/engine"
	"github.com/Yzx7/sacs-chatbots/models"
)

// GET /webhook/whatsapp — verificación del webhook (Meta).
func (con *Controller) WhatsAppVerify(c *fiber.Ctx) error {
	challenge, ok := whatsapp.VerifyChallenge(
		c.Query("hub.mode"),
		c.Query("hub.verify_token"),
		c.Query("hub.challenge"),
		con.Env.Config.WhatsAppVerifyToken,
	)
	if !ok {
		con.whatsAppLogger().Warn("verificación de webhook rechazada")
		return c.SendStatus(fiber.StatusForbidden)
	}
	con.whatsAppLogger().Info("webhook verificado")
	return c.SendString(challenge)
}

// POST /webhook/whatsapp — recibe eventos. Valida firma, responde 200 al instante
// y procesa en segundo plano (idempotente por wa_id).
func (con *Controller) WhatsAppWebhook(c *fiber.Ctx) error {
	body := append([]byte(nil), c.Body()...) // copia: el buffer de Fiber se reutiliza
	if !whatsapp.CheckSignature(con.Env.Config.WhatsAppAppSecret, body, c.Get("X-Hub-Signature-256")) {
		con.whatsAppLogger().Warn("webhook rechazado: firma inválida")
		return c.SendStatus(fiber.StatusForbidden)
	}
	con.whatsAppLogger().Info("webhook recibido", "bytes", len(body))
	go con.processWhatsApp(body)
	return c.SendStatus(fiber.StatusOK)
}

// handoffWindow es cuánto calla el bot tras que un humano toma la conversación.
const handoffWindow = 12 * time.Hour

// processWhatsApp normaliza, rutea por phone_number_id, ejecuta el flujo y responde.
func (con *Controller) processWhatsApp(body []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	pool := con.Env.Postgres
	cfg := con.Env.Config

	// Coexistence: ecos de mensajes salientes (incluye los que escribe una
	// persona desde la app de WhatsApp Business) → silencian al bot.
	con.handleEchoes(ctx, body)

	msgs, err := whatsapp.Parse(body)
	if err != nil {
		con.whatsAppLogger().Error("wa parse", "err", err.Error())
		return
	}

	for _, m := range msgs {
		bot, err := models.GetBotByChannel(ctx, pool, whatsapp.Channel, m.ChannelID)
		if err != nil || bot == nil {
			continue
		}
		chatID, err := models.UpsertChat(ctx, pool, bot.ID, m.From, m.ContactName)
		if err != nil {
			con.whatsAppLogger().Error("wa upsert chat", "err", err.Error())
			continue
		}
		if _, err := models.EnsureInboundContact(ctx, pool, bot.ID, m.From, m.ContactName); err != nil {
			con.whatsAppLogger().Error("wa ensure contact", "err", err.Error())
			continue
		}
		metadata, _ := json.Marshal(fiber.Map{"replyId": m.ReplyID, "mediaId": m.MediaID, "mimeType": m.MimeType, "caption": m.Caption})
		messageID, created, err := models.InsertInboundMessage(ctx, pool, chatID, m.WaID, string(m.Type), m.Text, metadata)
		if err != nil {
			con.whatsAppLogger().Error("wa insert message", "err", err.Error())
			continue
		}
		if !created {
			continue
		}

		// Handoff: si una persona tiene el chat, el bot guarda el mensaje pero no responde.
		if models.BotSilenced(ctx, pool, chatID) {
			continue
		}

		// Requiere canal conectado para responder.
		if con.Env.Cipher == nil || len(bot.TokenEnc) == 0 || bot.ChannelID == nil {
			continue
		}
		token, err := con.Env.Cipher.Decrypt(bot.TokenEnc)
		if err != nil {
			con.whatsAppLogger().Error("wa decrypt", "err", err.Error())
			continue
		}
		sendCfg := whatsapp.SendConfig{
			APIBase:       cfg.WhatsAppAPIBase,
			Version:       cfg.WhatsAppAPIVersion,
			PhoneNumberID: *bot.ChannelID,
			Token:         token,
		}
		if err := whatsapp.MarkMessageAsRead(ctx, sendCfg, m.WaID); err != nil {
			// La confirmación de lectura mejora la experiencia, pero no debe
			// impedir que el bot procese o responda el mensaje.
			con.whatsAppLogger().Warn("wa mark read", "message", m.WaID, "err", err.Error())
		}
		if m.Type == channels.MsgImage && m.MediaID != "" {
			data, mimeType, mediaErr := whatsapp.DownloadMedia(ctx, sendCfg, m.MediaID)
			if mediaErr != nil {
				con.whatsAppLogger().Error("wa download media", "err", mediaErr.Error())
			} else if err := models.SaveMessageMedia(ctx, pool, messageID, m.MediaID, mimeType, data); err != nil {
				con.whatsAppLogger().Error("wa save media", "err", err.Error())
			} else if m.MimeType == "" {
				m.MimeType = mimeType
			}
		}

		lockConn, err := pool.Acquire(ctx)
		if err != nil {
			continue
		}
		if _, err = lockConn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, chatID); err != nil {
			lockConn.Release()
			continue
		}
		replies, newState, handoff := con.runFlowOrEcho(ctx, bot, chatID, m, func() {
			if err := whatsapp.ShowTypingIndicator(ctx, sendCfg, m.WaID); err != nil {
				// El indicador es accesorio: un fallo no bloquea la respuesta.
				con.whatsAppLogger().Warn("wa typing indicator", "message", m.WaID, "err", err.Error())
			}
		})
		sentAll := true
		for _, txt := range replies {
			id, err := whatsapp.SendText(ctx, sendCfg, m.From, txt)
			if err != nil {
				if strings.Contains(err.Error(), "131037") {
					con.whatsAppLogger().Error("wa send: Meta bloqueó el número hasta aprobar su nombre para mostrar", "err", err.Error(), "action", "WhatsApp Manager > Números de teléfono > Nombre para mostrar")
				} else {
					con.whatsAppLogger().Error("wa send", "err", err.Error())
				}
				sentAll = false
				break
			}
			_ = models.InsertMessage(ctx, pool, chatID, id, true, "text", txt)
		}
		if sentAll && newState != nil {
			_ = models.SetChatState(ctx, pool, chatID, newState)
		}
		// El flujo escaló a un humano (action: handoff).
		if handoff {
			if err := models.HandoffChat(ctx, pool, chatID, handoffWindow); err != nil {
				con.whatsAppLogger().Error("handoff", "chat", chatID, "err", err.Error())
			} else {
				con.whatsAppLogger().Info("chat escalado a humano", "chat", chatID)
			}
		}
		_, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1,0))`, chatID)
		lockConn.Release()
	}
}

// handleEchoes procesa los ecos salientes (Coexistence). Si el eco NO corresponde
// a un mensaje que enviamos nosotros, lo escribió una persona desde la app de
// WhatsApp Business → pasamos el chat a modo manual para que el bot no interfiera.
func (con *Controller) handleEchoes(ctx context.Context, body []byte) {
	echoes, err := whatsapp.ParseEchoes(body)
	if err != nil || len(echoes) == 0 {
		return
	}
	pool := con.Env.Postgres
	for _, e := range echoes {
		if e.WaID == "" || e.To == "" {
			continue
		}
		bot, err := models.GetBotByChannel(ctx, pool, whatsapp.Channel, e.ChannelID)
		if err != nil || bot == nil {
			continue
		}
		// Si ya lo teníamos guardado, el mensaje salió del bot: nada que hacer.
		if exists, err := models.MessageExists(ctx, pool, e.WaID); err != nil || exists {
			continue
		}
		chatID, err := models.UpsertChat(ctx, pool, bot.ID, e.To, "")
		if err != nil {
			continue
		}
		_ = models.InsertMessage(ctx, pool, chatID, e.WaID, true, e.Type, e.Text)
		if err := models.HandoffChat(ctx, pool, chatID, handoffWindow); err != nil {
			con.whatsAppLogger().Error("handoff por eco", "chat", chatID, "err", err.Error())
			continue
		}
		con.whatsAppLogger().Info("humano tomó el chat desde la app (coexistence)", "chat", chatID)
	}
}

// runFlowOrEcho ejecuta el motor si el bot tiene flujo (trigger message); si no, eco.
// Devuelve los mensajes a enviar, el nuevo estado a persistir (nil = no tocar) y
// si el flujo pidió escalar a un humano.
func (con *Controller) runFlowOrEcho(ctx context.Context, bot *models.BotChannel, chatID string, m channels.InboundMessage, beforeProcess func()) ([]string, json.RawMessage, bool) {
	pool := con.Env.Postgres

	var flow engine.Flow
	hasFlow := len(bot.Flow) > 0 &&
		json.Unmarshal(bot.Flow, &flow) == nil &&
		len(flow.Nodes) > 0 && flow.Trigger.Type == "message"
	if !hasFlow {
		if m.Type == channels.MsgText {
			beforeProcess()
			return []string{"Recibí: " + m.Text}, nil, false
		}
		return nil, nil, false
	}

	// Estado previo de la conversación (chats.current_layer).
	var st *engine.State
	if raw, err := models.GetChatState(ctx, pool, chatID); err == nil && len(raw) > 0 {
		if s := string(raw); s != "null" && s != "[]" && s != "{}" {
			var parsed engine.State
			if json.Unmarshal(raw, &parsed) == nil && parsed.NodeID != "" {
				st = &parsed
			}
		}
	}
	if st == nil && !engine.TriggerMatches(flow.Trigger, m.Text) {
		return nil, nil, false
	}
	beforeProcess()

	// Inyecta el nodo IA (MiniMax) si está configurado.
	deps := engine.Deps{Context: models.FlowContext(ctx, pool, bot.ID, m.From), InputType: string(m.Type), MediaID: m.MediaID, WaID: m.WaID}
	if con.Env.Agent != nil {
		deps.Agent = func(instruction string, vars map[string]string, outputs []string) (string, string, error) {
			return con.Env.Agent.Run(ctx, instruction, vars, outputs)
		}
	}
	deps.Tool = func(ref string, args, vars map[string]string) (string, error) {
		switch ref {
		case "record_payment_receipt":
			return models.RecordPaymentReceipt(ctx, pool, bot.ID, m.From, vars["input_wa_id"], vars["input_media_id"], m.MimeType)
		default:
			return "", fmt.Errorf("herramienta %q no implementada", ref)
		}
	}
	res, err := engine.Advance(&flow, st, m.Text, deps)
	if err != nil {
		con.whatsAppLogger().Error("engine advance", "err", err.Error())
		return []string{"No pude procesar tu solicitud en este momento. Intenta nuevamente o escribe *asesor*."}, nil, false
	}

	stateRaw := json.RawMessage("null")
	if res.State != nil {
		if b, e := json.Marshal(res.State); e == nil {
			stateRaw = b
		}
	}
	return res.Sends, stateRaw, res.Handoff
}

func (con *Controller) whatsAppLogger() *slog.Logger {
	if con.Env.WhatsAppLogger != nil {
		return con.Env.WhatsAppLogger
	}
	return con.Env.Logger
}
