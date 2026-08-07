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
	"github.com/Yzx7/sacs-chatbots/engine/ai"
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
	con.handleTemplateEvents(ctx, body)
	con.handleStatuses(ctx, body)
	con.handleEchoes(ctx, body)
	con.handleAccountEvents(ctx, body)
	con.handleUserPreferences(ctx, body)
	con.warnUnhandledFields(body)

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
		metadata, _ := json.Marshal(fiber.Map{
			"eventType": m.EventType, "replyId": m.ReplyID, "quotedWaId": m.QuotedWaID,
			"mediaId": m.MediaID, "mimeType": m.MimeType, "mediaSha256": m.MediaSHA256,
			"mediaUrl": m.MediaURL, "fileName": m.FileName, "caption": m.Caption,
			"voice": m.Voice, "animated": m.Animated, "forwarded": m.Forwarded,
			"frequentlyForwarded": m.FrequentlyForwarded, "location": m.Location,
			"contacts": m.Contacts, "order": m.Order,
			"reactionEmoji": m.ReactionEmoji, "reactionMessageId": m.ReactionMessageID,
			"reactionRemoved": m.ReactionRemoved, "raw": m.Raw,
		})
		messageID, created, err := models.InsertInboundMessage(ctx, pool, chatID, m.WaID, string(m.Type), m.Text, metadata)
		if err != nil {
			con.whatsAppLogger().Error("wa insert message", "err", err.Error())
			continue
		}
		if !created {
			continue
		}
		correlation, err := models.CorrelateInboundReminder(ctx, pool, messageID, chatID,
			m.QuotedWaID, cfg.ReminderCorrelationWindow)
		if err != nil {
			con.whatsAppLogger().Error("wa correlate reminder", "message", m.WaID, "err", err.Error())
			correlation = nil
		}

		// Requiere canal conectado para descargar medios y responder. Sin él, la
		// bandeja al menos ve el texto.
		if con.Env.Cipher == nil || len(bot.TokenEnc) == 0 || bot.ChannelID == nil {
			con.publishInbound(ctx, bot.ID, chatID, messageID, m, false)
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
		// El medio se descarga siempre (aunque atienda un humano): así la bandeja y
		// las tools no dependen de un media_id que expira.
		hasMedia := false
		if m.HasMedia() {
			data, mimeType, mediaErr := whatsapp.DownloadMedia(ctx, sendCfg, m.MediaID)
			if mediaErr != nil {
				con.whatsAppLogger().Error("wa download media", "err", mediaErr.Error())
			} else if err := models.SaveMessageMedia(ctx, pool, messageID, m.MediaID, mimeType, data); err != nil {
				con.whatsAppLogger().Error("wa save media", "err", err.Error())
			} else {
				hasMedia = true
				if m.MimeType == "" {
					m.MimeType = mimeType
				}
			}
		}
		con.publishInbound(ctx, bot.ID, chatID, messageID, m, hasMedia)

		// Handoff: si una persona tiene el chat, el bot guarda el mensaje pero no responde.
		if models.BotSilenced(ctx, pool, chatID) {
			continue
		}

		lockConn, err := pool.Acquire(ctx)
		if err != nil {
			continue
		}
		if _, err = lockConn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, chatID); err != nil {
			lockConn.Release()
			continue
		}
		replies, newState, handoff := con.runFlowOrEcho(ctx, bot, chatID, messageID, m, correlation, func() {
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
			if saved, err := models.InsertOutboundMessage(ctx, pool, chatID, id, "text", txt); err != nil {
				con.whatsAppLogger().Error("wa guardar respuesta", "err", err.Error())
			} else if saved != nil {
				con.publishChat(ctx, "message", bot.ID, chatID, saved)
				con.reconcileStatusEvents(ctx, id)
			}
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
				con.publishMode(ctx, bot.ID, chatID)
			}
		}
		_, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1,0))`, chatID)
		lockConn.Release()
	}
}

// handleTemplateEvents conserva y aplica los cambios de estado, calidad y
// categoría de las plantillas. Los payloads usan entry.id como WABA ID.
func (con *Controller) handleTemplateEvents(ctx context.Context, body []byte) {
	events, err := whatsapp.ParseTemplateEvents(body)
	if err != nil || len(events) == 0 {
		return
	}
	for _, event := range events {
		applied, err := models.StoreAndApplyTemplateEvent(ctx, con.Env.Postgres, event)
		if err != nil {
			con.whatsAppLogger().Error("wa template event", "field", event.Field,
				"template", event.Name, "err", err.Error())
			continue
		}
		if applied {
			con.whatsAppLogger().Info("wa template actualizado", "field", event.Field,
				"waba", event.WabaID, "template", event.Name, "status", event.Status,
				"category", event.Category, "pendingCategory", event.PendingCategory)
		}
	}
}

// handleAccountEvents conserva y aplica la salud del canal: restricciones,
// infracciones, calidad del número, límite de mensajería, revisión de la cuenta
// y aprobación del nombre para mostrar.
//
// Los seis campos llevaban suscritos en Meta desde el alta de la app **sin
// receptor**: llegaban y se descartaban. Con Coexistence eso significaba que una
// desconexión del teléfono solo se notaba porque dejaban de entrar mensajes.
func (con *Controller) handleAccountEvents(ctx context.Context, body []byte) {
	events, err := whatsapp.ParseAccountEvents(body)
	if err != nil || len(events) == 0 {
		return
	}
	for _, event := range events {
		applied, err := models.StoreAndApplyAccountEvent(ctx, con.Env.Postgres, event)
		if err != nil {
			con.whatsAppLogger().Error("wa account event", "field", event.Field,
				"waba", event.WabaID, "err", err.Error())
			continue
		}
		if !applied {
			continue
		}
		// El nivel del log sigue a la severidad para que un `grep level=ERROR`
		// encuentre una restricción sin tener que saber qué campo la trae.
		log := con.whatsAppLogger()
		args := []any{"field", event.Field, "waba", event.WabaID,
			"phone", event.PhoneNumberID, "event", event.Event,
			"limit", event.MessagingLimit, "decision", event.Decision}
		switch event.Severity {
		case whatsapp.SeverityCritical:
			log.Error("salud del canal: evento crítico", args...)
		case whatsapp.SeverityWarning:
			log.Warn("salud del canal", args...)
		default:
			log.Info("salud del canal", args...)
		}
	}
}

// handleUserPreferences registra el opt-out y el opt-in de marketing.
//
// Se guarda contra el **contacto**, que es la identidad por (org, teléfono), y no
// contra el chat: la voluntad es de la persona. Si escribe a dos bots de la misma
// organización, su decisión vale para ambos.
//
// Hoy no cambia ningún envío —las plantillas de cobranza son UTILITY y el opt-out
// promocional no las afecta— y es justo por eso que hay que registrarlo ya: un
// opt-out perdido no se recupera, y el registro tiene que existir antes del
// primer envío de marketing, no después.
func (con *Controller) handleUserPreferences(ctx context.Context, body []byte) {
	prefs, err := whatsapp.ParseUserPreferences(body)
	if err != nil || len(prefs) == 0 {
		return
	}
	pool := con.Env.Postgres
	for _, pref := range prefs {
		bot, err := models.GetBotByChannel(ctx, pool, whatsapp.Channel, pref.ChannelID)
		if err != nil {
			// No es lo mismo «este número no es nuestro» que «no pudimos
			// averiguarlo». Colapsar ambos en un WARN escondería la pérdida de un
			// opt-out real por un fallo transitorio de base de datos, y como el
			// webhook ya respondió 200, Meta no lo reintenta: se perdería.
			con.whatsAppLogger().Error("preferencia: no se pudo resolver el canal",
				"channel", pref.ChannelID, "category", pref.Category, "err", err.Error())
			continue
		}
		if bot == nil {
			con.whatsAppLogger().Warn("preferencia de un canal desconocido",
				"channel", pref.ChannelID, "category", pref.Category, "value", pref.Value)
			continue
		}
		// Se crea el contacto si no existe: alguien puede pedir el alta de baja
		// sin habernos escrito nunca por este bot, y perder ese "no" por no
		// tenerlo fichado sería el peor de los fallos posibles aquí.
		contact, err := models.EnsureInboundContact(ctx, pool, bot.ID, pref.WaID, "")
		if err != nil || contact == nil {
			con.whatsAppLogger().Error("preferencia: no se pudo resolver el contacto",
				"bot", bot.ID, "category", pref.Category, "err", errText(err))
			continue
		}
		applied, err := models.StoreAndApplyUserPreference(ctx, pool, contact.ID, pref)
		if err != nil {
			con.whatsAppLogger().Error("preferencia: no se pudo guardar",
				"contact", contact.ID, "category", pref.Category, "err", err.Error())
			continue
		}
		if !applied {
			continue
		}
		con.whatsAppLogger().Info("preferencia del usuario actualizada",
			"contact", contact.ID, "category", pref.Category, "value", pref.Value,
			"optedOut", pref.OptedOut())
	}
}

func errText(err error) string {
	if err == nil {
		return "contacto no resuelto"
	}
	return err.Error()
}

// warnUnhandledFields deja constancia de todo campo suscrito que ningún parser
// reclama. Sin esto no hay forma de saber qué se está perdiendo: el webhook
// responde 200, el log dice "webhook recibido" con el tamaño en bytes y ahí
// muere. Fue justo lo que ocultó durante semanas seis campos sin receptor.
//
// No se vuelca el `value`: puede traer teléfono, nombre y texto del cliente, y
// los logs no son el sitio. La carga íntegra va a channel_account_events, que
// está en Postgres y bajo la organización dueña.
func (con *Controller) warnUnhandledFields(body []byte) {
	var payload struct {
		Entry []struct {
			Changes []struct {
				Field string          `json:"field"`
				Value json.RawMessage `json:"value"`
			} `json:"changes"`
		} `json:"entry"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return
	}
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			if change.Field == "" || handledWebhookFields[change.Field] {
				continue
			}
			con.whatsAppLogger().Warn("campo de webhook sin receptor: se descarta",
				"field", change.Field, "bytes", len(change.Value))
		}
	}
}

// handledWebhookFields son los campos que algún parser reclama. Añadir un
// receptor obliga a añadirlo aquí; si no, warnUnhandledFields lo denunciará en
// cada evento, que es exactamente el recordatorio que se busca.
var handledWebhookFields = func() map[string]bool {
	fields := map[string]bool{
		"messages":                        true, // entrantes + estados de entrega
		"smb_message_echoes":              true,
		"message_echoes":                  true, // no suscrito; el parser lo aceptaría
		"message_template_status_update":  true,
		"message_template_quality_update": true,
		"template_category_update":        true,
		whatsapp.PreferenceField:          true,
	}
	for field := range whatsapp.AccountFields {
		fields[field] = true
	}
	return fields
}()

// handleStatuses persiste primero el evento y luego intenta reconciliarlo. Si
// el status ganó la carrera al INSERT del mensaje, queda pendiente sin perderse.
func (con *Controller) handleStatuses(ctx context.Context, body []byte) {
	statuses, err := whatsapp.ParseStatuses(body)
	if err != nil || len(statuses) == 0 {
		return
	}
	for _, status := range statuses {
		metadata, _ := json.Marshal(status)
		_, err := models.StoreProviderStatusEvent(ctx, con.Env.Postgres, models.ProviderStatusEventInput{
			Channel: whatsapp.Channel, ChannelID: status.ChannelID,
			ProviderMessageID: status.MessageID, Status: status.Status, OccurredAt: status.OccurredAt,
			RecipientID: status.RecipientID, ErrorCode: status.ErrorCode, ErrorTitle: status.ErrorTitle,
			ErrorMessage: status.ErrorMessage, ErrorDetails: status.ErrorDetails,
			ConversationID: status.ConversationID, ConversationType: status.ConversationType,
			PricingModel: status.PricingModel, PricingType: status.PricingType,
			PricingCategory: status.PricingCategory, Billable: status.Billable,
			OpaqueCallback: status.OpaqueCallback, Metadata: metadata,
		})
		if err != nil {
			con.whatsAppLogger().Error("wa status persist", "message", status.MessageID, "err", err.Error())
			continue
		}
		con.reconcileStatusEvents(ctx, status.MessageID)
	}
}

func (con *Controller) reconcileStatusEvents(ctx context.Context, providerMessageID string) {
	updates, err := models.ReconcileProviderStatusEvents(ctx, con.Env.Postgres, providerMessageID)
	if err != nil {
		con.whatsAppLogger().Error("wa status reconcile", "message", providerMessageID, "err", err.Error())
		return
	}
	for _, update := range updates {
		con.publishChat(ctx, "message_status", update.BotID, update.ChatID, update)
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
		// El chat se resuelve antes que nada porque hace falta su id para tomar
		// el lock: la decisión de si el eco es humano no puede tomarse sin él.
		chatID, err := models.UpsertChat(ctx, pool, bot.ID, e.To, "")
		if err != nil {
			continue
		}
		con.applyEcho(ctx, bot, chatID, e)
	}
}

// applyEcho decide si un eco lo escribió una persona y, en ese caso, silencia al
// bot. Corre bajo el **mismo** advisory lock por chat que protege el envío del
// motor, y de ahí que esté en su propia función: así el lock se libera por todos
// los caminos de salida.
//
// El lock no es una precaución teórica. La respuesta del bot se manda a Meta y
// solo después se guarda su wa_id, así que hay un hueco en el que `MessageExists`
// diría que el mensaje no es nuestro. Un eco que cayera ahí haría que el bot se
// diese handoff a sí mismo y se callara 12 h sin un solo error en el log.
// Esperar al lock convierte esa carrera en una espera de milisegundos.
func (con *Controller) applyEcho(ctx context.Context, bot *models.BotChannel, chatID string, e whatsapp.Echo) {
	pool := con.Env.Postgres
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		con.whatsAppLogger().Error("eco: no se pudo tomar conexión", "chat", chatID, "err", err.Error())
		return
	}
	defer lockConn.Release()
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, chatID); err != nil {
		// Se descarta en vez de seguir sin lock: procesarlo aquí es exactamente
		// la carrera que esta función existe para evitar.
		con.whatsAppLogger().Error("eco: no se pudo tomar el lock del chat", "chat", chatID, "err", err.Error())
		return
	}
	// Contexto propio: el unlock debe ocurrir aunque ctx ya esté cancelado, o la
	// conexión volvería al pool con el lock puesto.
	defer lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1,0))`, chatID)

	// Si ya lo teníamos guardado, el mensaje salió del bot: nada que hacer.
	if exists, err := models.MessageExists(ctx, pool, e.WaID); err != nil || exists {
		return
	}
	// Se guarda y se publica para que la bandeja muestre lo que el agente
	// escribió desde el teléfono, no solo lo que sale del bot.
	if saved, err := models.InsertOutboundMessage(ctx, pool, chatID, e.WaID, e.Type, e.Text); err == nil && saved != nil {
		con.publishChat(ctx, "message", bot.ID, chatID, saved)
		con.reconcileStatusEvents(ctx, e.WaID)
	}
	if err := models.HandoffChat(ctx, pool, chatID, handoffWindow); err != nil {
		con.whatsAppLogger().Error("handoff por eco", "chat", chatID, "err", err.Error())
		return
	}
	con.whatsAppLogger().Info("humano tomó el chat desde la app (coexistence)", "chat", chatID)
	con.publishMode(ctx, bot.ID, chatID)
}

// runFlowOrEcho ejecuta el motor si el bot tiene flujo (trigger message); si no, eco.
// Devuelve los mensajes a enviar, el nuevo estado a persistir (nil = no tocar) y
// si el flujo pidió escalar a un humano.
func (con *Controller) runFlowOrEcho(ctx context.Context, bot *models.BotChannel, chatID string, inboundMessageID int64, m channels.InboundMessage, correlation *models.ReminderCorrelation, beforeProcess func()) ([]string, json.RawMessage, bool) {
	pool := con.Env.Postgres

	// Estado previo de la conversación (chats.current_layer). Se lee **antes** de
	// elegir el flujo: una conversación a medias manda sobre el despacho.
	var st *engine.State
	if raw, err := models.GetChatState(ctx, pool, chatID); err == nil && len(raw) > 0 {
		if s := string(raw); s != "null" && s != "[]" && s != "{}" {
			var parsed engine.State
			if json.Unmarshal(raw, &parsed) == nil && parsed.NodeID != "" {
				st = &parsed
			}
		}
	}
	stateFlowID := ""
	if st != nil {
		stateFlowID = st.FlowID
	}

	selected := con.messageFlowForInput(ctx, bot.ID, stateFlowID, m.Text)

	var flow engine.Flow
	hasFlow := selected != nil &&
		json.Unmarshal(selected.Definition, &flow) == nil &&
		len(flow.Nodes) > 0 && flow.Trigger.Type == "message"
	if !hasFlow {
		if m.Type == channels.MsgText {
			beforeProcess()
			return []string{"Recibí: " + m.Text}, nil, false
		}
		return nil, nil, false
	}

	// Un estado de otro flujo no se reanuda: sus nodeId y variables pertenecen a
	// un grafo distinto. Se arranca de cero en el flujo elegido.
	if st != nil && st.FlowID != "" && st.FlowID != selected.FlowID {
		st = nil
	}
	if st == nil && !engine.TriggerMatchesInput(flow.Trigger, m.Text, string(m.Type)) {
		return nil, nil, false
	}
	if shouldBlockAmbiguousReceipt(correlation, m.Type) {
		beforeProcess()
		return []string{"Encontré más de un aviso reciente y no puedo atribuir este archivo con seguridad. Responde directamente al aviso correcto y vuelve a enviar la imagen para asociarla sin errores."}, nil, false
	}
	beforeProcess()

	// Inyecta el nodo IA si hay al menos un proveedor configurado. La capacidad
	// declarada por cada nodo decide después: image → MiniMax-M3; resto → DeepSeek.
	flowContext := models.FlowContext(ctx, pool, bot.ID, m.From)
	flowContext["source_intent"] = ""
	if correlated, err := models.CorrelationContext(ctx, pool, bot.ID, correlation); err != nil {
		con.whatsAppLogger().Error("wa correlation context", "message", m.WaID, "err", err.Error())
	} else {
		for key, value := range correlated {
			flowContext[key] = value
		}
	}
	mediaSource := &agentMediaSource{
		MessageID: inboundMessageID,
		Inbound:   m,
	}
	deps := engine.Deps{
		Context: flowContext, InputType: string(m.Type), MediaID: m.MediaID, WaID: m.WaID,
		Input: engine.InboundInput{
			ID: m.WaID, EventType: string(m.EventType), ContentType: string(m.Type), Text: m.Text,
			Caption: m.Caption, MediaID: m.MediaID, MimeType: m.MimeType,
			MediaSHA256: m.MediaSHA256, ReplyTo: m.QuotedWaID,
			Forwarded: m.Forwarded, FrequentlyForwarded: m.FrequentlyForwarded,
			ReactionEmoji: m.ReactionEmoji, ReactionMessageID: m.ReactionMessageID,
			ReactionRemoved: m.ReactionRemoved,
		},
	}
	if con.Env.TextAgent != nil || con.Env.VisionAgent != nil {
		deps.AgentStructured = func(request engine.AgentRequest) (engine.AgentResult, error) {
			nodeID := request.NodeID
			if mediaErr := con.attachCurrentAgentMedia(ctx, bot, &request, mediaSource); mediaErr != nil {
				return engine.AgentResult{}, mediaErr
			}
			agentTools, toolExec, toolErr := con.agentTooling(ctx, bot, request.Tools)
			if toolErr != nil {
				return engine.AgentResult{}, toolErr
			}
			for attempt := 1; attempt <= 2; attempt++ {
				startedAt := time.Now()
				// Sin herramientas se conserva el camino de una sola petición: un
				// bucle para un nodo que no puede llamar nada solo añadiría coste.
				agentResult, usage, runErr := con.runAgent(ctx, bot.OrgID, request, agentTools, toolExec)
				duration := time.Since(startedAt)
				errorCode := ai.OutputErrorCode(runErr)
				if usage.Provider != "" {
					outcome := "ok"
					if runErr != nil {
						outcome = "invalid_output"
					}
					usageMetadata := map[string]any{
						"source":      "flow_agent",
						"node_id":     nodeID,
						"attempt":     fmt.Sprint(attempt),
						"duration_ms": fmt.Sprint(duration.Milliseconds()),
					}
					if errorCode != "" {
						usageMetadata["error_code"] = errorCode
					}
					if agentResult.Branch != "" {
						usageMetadata["branch"] = agentResult.Branch
					}
					// Cuántas peticiones costó el turno. Con herramientas deja de ser
					// 1, y es el número que explica una factura que sube sin que
					// cambie el tráfico.
					if usage.Steps > 1 {
						usageMetadata["steps"] = fmt.Sprint(usage.Steps)
					}
					if len(usage.ToolTrace) > 0 {
						// Solo se guardan nombres y resultado operativo. Los argumentos y
						// resultados pueden contener datos del cliente y no pertenecen a
						// la telemetría de consumo.
						usageMetadata["tool_trace"] = usage.ToolTrace
					}
					metadata, _ := json.Marshal(usageMetadata)
					if err := models.RecordAIUsage(ctx, pool, models.AIUsageEventInput{
						OrganizationID: bot.OrgID, BotID: bot.ID, ChatID: chatID,
						InboundMessageID: inboundMessageID, Provider: usage.Provider,
						Model: usage.Model, ProviderRequestID: usage.RequestID,
						InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
						CacheReadInputTokens:     usage.CacheReadInputTokens,
						CacheCreationInputTokens: usage.CacheCreationInputTokens,
						InputUSDPerMillion:       usage.Rates.InputPerMillion,
						OutputUSDPerMillion:      usage.Rates.OutputPerMillion,
						CacheReadUSDPerMillion:   usage.Rates.CacheReadPerMillion,
						CacheWriteUSDPerMillion:  usage.Rates.CacheWritePerMillion,
						Outcome:                  outcome, Metadata: metadata,
					}); err != nil {
						con.whatsAppLogger().Error("ai usage persist", "request", usage.RequestID, "err", err.Error())
					}
				}

				logArgs := []any{
					"bot_id", bot.ID,
					"chat_id", chatID,
					"node_id", nodeID,
					"attempt", attempt,
					"duration_ms", duration.Milliseconds(),
					"model", usage.Model,
					"branch", agentResult.Branch,
					"silent", request.Silent,
					"tools", len(agentTools),
				}
				if runErr == nil || attempt == 2 || !retryableAgentOutput(errorCode) {
					if runErr == nil {
						con.whatsAppLogger().Info("ai route selected", logArgs...)
					} else {
						con.whatsAppLogger().Warn("ai route failed", append(logArgs, "error_code", errorCode)...)
					}
					return agentResult, runErr
				}
				con.whatsAppLogger().Warn(
					"ai structured output retry",
					"bot_id", bot.ID,
					"chat_id", chatID,
					"node_id", nodeID,
					"error_code", errorCode,
					"request", usage.RequestID,
					"duration_ms", duration.Milliseconds(),
				)
			}
			return engine.AgentResult{}, fmt.Errorf("agente IA agotó sus intentos")
		}
	}
	deps.Tool = func(ref string, args, _ map[string]string) (string, error) {
		switch ref {
		case "data_mutate":
			return con.execDataMutate(ctx, bot, m.From, m.WaID, args)
		case "data_query":
			return con.execDataQuery(ctx, bot, m.From, args)
		default:
			return "", fmt.Errorf("herramienta %q no implementada", ref)
		}
	}
	res, err := engine.Advance(&flow, st, m.Text, deps)
	stateRaw := json.RawMessage("null")
	if res.State != nil {
		// El motor no conoce la tabla `flows`: el dueño del estado lo sella aquí,
		// para que el siguiente mensaje reanude en este mismo grafo.
		res.State.FlowID = selected.FlowID
		if b, marshalErr := json.Marshal(res.State); marshalErr == nil {
			stateRaw = b
		}
	}
	if err != nil {
		con.whatsAppLogger().Error("engine advance", "error_code", ai.OutputErrorCode(err), "err", err.Error())
		return []string{"No pude procesar tu solicitud en este momento. Intenta nuevamente o escribe *asesor*."}, stateRaw, false
	}
	return res.Sends, stateRaw, res.Handoff
}

func shouldBlockAmbiguousReceipt(correlation *models.ReminderCorrelation, messageType channels.MessageType) bool {
	return correlation != nil && correlation.Method == "ambiguous" && messageType == channels.MsgImage
}

func retryableAgentOutput(errorCode string) bool {
	switch errorCode {
	case "missing_tool_call", "unexpected_tool", "multiple_tool_calls",
		"invalid_tool_input", "invalid_branch", "empty_reply", "missing_data",
		"invalid_data", "unknown_data_field", "invalid_data_type":
		return true
	default:
		return false
	}
}

func (con *Controller) whatsAppLogger() *slog.Logger {
	if con.Env.WhatsAppLogger != nil {
		return con.Env.WhatsAppLogger
	}
	return con.Env.Logger
}
