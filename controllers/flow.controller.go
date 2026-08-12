package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/engine"
	authmw "github.com/Yzx7/sacs-chatbots/middlewares/auth"
	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
)

// Flujos del bot (§10.1 del plan). La autorización se resuelve por bot → org con
// botWithRole, igual que el resto del backend: no se denormaliza org_id en flows.
//
// En esta fase la escritura del flujo que ejecuta el bot sigue yendo a
// `bots.flow` (PUT /bots/:botId/flow). Estos endpoints construyen el lugar donde
// alojar los flujos `schedule`; §12 paso 7 mueve la escritura más adelante.

// currentUserID devuelve el id del usuario autenticado ("" si no hay claims).
func (con *Controller) currentUserID(c *fiber.Ctx) string {
	if claims, ok := authmw.Current(c); ok {
		return claims.UserID
	}
	return ""
}

// flowWithRole carga el flujo comprobando permisos sobre la org dueña del bot.
func (con *Controller) flowWithRole(c *fiber.Ctx, roles ...string) (*models.Bot, *models.Flow, error) {
	bot, err := con.botWithRole(c, c.Params("botId"), roles...)
	if err != nil {
		return nil, nil, err
	}
	flow, err := models.GetFlow(c.Context(), con.Env.Postgres, bot.ID, c.Params("flowId"))
	if err != nil {
		return nil, nil, fiber.NewError(fiber.StatusInternalServerError, "error obteniendo el flujo")
	}
	if flow == nil {
		return nil, nil, fiber.NewError(fiber.StatusNotFound, "flujo no encontrado")
	}
	return bot, flow, nil
}

// GET /bots/:botId/flows — lista de flujos (cualquier miembro).
func (con *Controller) ListBotFlows(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"))
	if err != nil {
		return con.failErr(c, err)
	}
	flows, err := models.ListFlows(c.Context(), con.Env.Postgres, bot.ID, c.QueryBool("archived", false))
	if err != nil {
		con.Env.Logger.Error("ListFlows", "botId", bot.ID, "err", err.Error())
		return con.fail(c, fiber.StatusInternalServerError, "error obteniendo los flujos")
	}
	return con.ok(c, "ok", flows)
}

// POST /bots/:botId/flows — crea un flujo en borrador (owner/admin/member).
func (con *Controller) CreateBotFlow(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	var b struct {
		Key         string          `json:"key"`
		Name        string          `json:"name"`
		TriggerType string          `json:"triggerType"`
		Priority    int             `json:"priority"`
		IsFallback  bool            `json:"isFallback"`
		Draft       json.RawMessage `json:"draft"`
	}
	if err := c.BodyParser(&b); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido")
	}
	b.Key = strings.TrimSpace(strings.ToLower(b.Key))
	b.Name = strings.TrimSpace(b.Name)
	b.TriggerType = strings.TrimSpace(b.TriggerType)
	if b.Name == "" {
		return con.fail(c, fiber.StatusBadRequest, "el nombre es obligatorio")
	}
	if !models.ValidFlowKey(b.Key) {
		return con.fail(c, fiber.StatusBadRequest, models.ErrFlowInvalidKey.Error())
	}
	switch b.TriggerType {
	case "message", "schedule", "event":
	default:
		return con.fail(c, fiber.StatusBadRequest, "triggerType debe ser message, schedule o event")
	}
	if len(b.Draft) > 0 && !json.Valid(b.Draft) {
		return con.fail(c, fiber.StatusBadRequest, "el borrador no es JSON válido")
	}
	// La creación acepta `priority` e `isFallback` en el cuerpo, así que endurecer
	// solo UpdateBotFlowMeta dejaría la puerta de atrás abierta: bastaría con
	// pedirlos al crear el flujo. `priority` 0 se trata como "no lo pidió", que es
	// lo que ya hace CreateFlow al sustituirlo por el 100 por defecto.
	if b.IsFallback || (b.Priority != 0 && b.Priority != 100) {
		if _, err := con.requireOrgRole(c, bot.OrgID, "owner", "admin"); err != nil {
			return con.fail(c, fiber.StatusForbidden, "fijar la prioridad o el flujo de reserva requiere un administrador")
		}
	}

	flow, err := models.CreateFlow(c.Context(), con.Env.Postgres, bot.ID, models.NewFlow{
		Key:         b.Key,
		Name:        b.Name,
		TriggerType: b.TriggerType,
		Priority:    b.Priority,
		IsFallback:  b.IsFallback,
		Draft:       b.Draft,
		UserID:      con.currentUserID(c),
	})
	if err != nil {
		return con.failFlow(c, "CreateFlow", bot.ID, err, "no se pudo crear el flujo")
	}
	return c.Status(fiber.StatusCreated).JSON(types.OK("flujo creado", flow))
}

// GET /bots/:botId/flows/:flowId — detalle (cualquier miembro).
func (con *Controller) GetBotFlowByID(c *fiber.Ctx) error {
	_, flow, err := con.flowWithRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	capability := con.flowCopilotCapability()
	flow.CopilotCapability = &capability
	return con.ok(c, "ok", flow)
}

// GET /bots/:botId/flows/:flowId/draft — borrador editable.
func (con *Controller) GetBotFlowDraft(c *fiber.Ctx) error {
	_, flow, err := con.flowWithRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	snapshot, err := models.DraftSnapshotFromFlow(flow)
	if err != nil {
		return con.failFlow(c, "DraftSnapshotFromFlow", flow.BotID, err, "no se pudo leer el borrador")
	}
	return con.ok(c, "ok", snapshot)
}

// PUT /bots/:botId/flows/:flowId/draft — guarda el borrador (owner/admin/member).
// El borrador puede estar incompleto: la validación dura es al publicar.
func (con *Controller) UpdateBotFlowDraft(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	if flow.ArchivedAt != nil {
		return con.fail(c, fiber.StatusConflict, models.ErrFlowArchived.Error())
	}
	draft, expectedChecksum, problem := parseDraftUpdateBody(c.Body())
	if problem != "" {
		return con.fail(c, fiber.StatusBadRequest, problem)
	}
	updated, err := models.UpdateFlowDraft(c.Context(), con.Env.Postgres, bot.ID, flow.ID,
		draft, expectedChecksum, con.currentUserID(c))
	if err != nil {
		return con.failFlow(c, "UpdateFlowDraft", bot.ID, err, "no se pudo guardar el borrador")
	}
	if updated == nil {
		return con.fail(c, fiber.StatusNotFound, "flujo no encontrado")
	}
	return con.ok(c, "borrador guardado", updated)
}

// PATCH /bots/:botId/flows/:flowId — nombre, prioridad y fallback (owner/admin/member).
//
// No toca el grafo: renombrar o cambiar quién es el fallback no debe obligar a
// republicar. Los campos ausentes conservan su valor, para que la pantalla pueda
// mandar solo lo que el operador cambió.
func (con *Controller) UpdateBotFlowMeta(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	if flow.ArchivedAt != nil {
		return con.fail(c, fiber.StatusConflict, models.ErrFlowArchived.Error())
	}
	var b struct {
		Name       *string `json:"name"`
		Priority   *int    `json:"priority"`
		IsFallback *bool   `json:"isFallback"`
	}
	if err := c.BodyParser(&b); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido")
	}
	name, priority, isFallback := flow.Name, flow.Priority, flow.IsFallback
	if b.Name != nil {
		if name = strings.TrimSpace(*b.Name); name == "" {
			return con.fail(c, fiber.StatusBadRequest, "el nombre es obligatorio")
		}
	}
	// Prioridad y fallback deciden **el orden del despacho de todo el bot**, no
	// solo el de este flujo: bajar la prioridad de un flujo propio le roba el
	// turno a otro, y marcarse fallback desmarca al que lo era. Un `member` puede
	// construir su flujo entero sin tocar ninguna de las dos.
	if b.Priority != nil && *b.Priority != flow.Priority {
		if _, err := con.requireOrgRole(c, bot.OrgID, "owner", "admin"); err != nil {
			return con.fail(c, fiber.StatusForbidden, "cambiar la prioridad reordena el despacho del bot y requiere un administrador")
		}
		if *b.Priority < 0 || *b.Priority > 1000 {
			return con.fail(c, fiber.StatusBadRequest, "la prioridad va de 0 a 1000")
		}
		priority = *b.Priority
	}
	if b.IsFallback != nil && *b.IsFallback != flow.IsFallback {
		if _, err := con.requireOrgRole(c, bot.OrgID, "owner", "admin"); err != nil {
			return con.fail(c, fiber.StatusForbidden, "marcar el flujo de reserva requiere un administrador")
		}
		if *b.IsFallback && flow.TriggerType != "message" {
			return con.fail(c, fiber.StatusBadRequest, "solo un flujo de conversación puede ser el fallback")
		}
		if *b.IsFallback && len(flow.Audience) > 0 {
			return con.fail(c, fiber.StatusBadRequest, models.ErrFlowAudienceFallback.Error())
		}
		isFallback = *b.IsFallback
	}

	updated, err := models.UpdateFlowMeta(c.Context(), con.Env.Postgres, bot.ID, flow.ID,
		name, priority, isFallback, con.currentUserID(c))
	if err != nil {
		return con.failFlow(c, "UpdateFlowMeta", bot.ID, err, "no se pudo actualizar el flujo")
	}
	if updated == nil {
		return con.fail(c, fiber.StatusNotFound, "flujo no encontrado")
	}
	return con.ok(c, "flujo actualizado", updated)
}

// PUT /bots/:botId/flows/:flowId/audience — restringe a quién atiende el flujo
// (owner/admin). Un cuerpo vacío, `null` o `{}` retira la restricción.
//
// **Endpoint propio y no un campo del PATCH de metadatos**, y la razón es de
// seguridad y no de estética: si la audiencia viajara junto al nombre, un
// `member` —que sí puede renombrar— podría quitarse la restricción y publicar
// después sin ella, saltándose justo el permiso que PublishBotFlow impone. Ese
// es el agujero que este endpoint existe para cerrar.
//
// No exige republicar. La audiencia es metadato operativo —quién entra al
// grafo—, de la misma familia que `priority` e `is_fallback`, que ya viven en la
// fila mutable y ya surten efecto sin versión nueva.
func (con *Controller) SetBotFlowAudience(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin")
	if err != nil {
		return con.failErr(c, err)
	}
	if flow.ArchivedAt != nil {
		return con.fail(c, fiber.StatusConflict, models.ErrFlowArchived.Error())
	}
	body := c.Body()
	if len(body) > 0 && !json.Valid(body) {
		return con.fail(c, fiber.StatusBadRequest, "la condición de audiencia no es JSON válido")
	}
	updated, err := models.SetFlowAudience(c.Context(), con.Env.Postgres, bot.ID, flow.ID,
		json.RawMessage(body), con.currentUserID(c))
	if err != nil {
		switch {
		case errors.Is(err, models.ErrFlowAudienceFallback), errors.Is(err, models.ErrFlowAudienceOnMessage):
			return con.fail(c, fiber.StatusBadRequest, err.Error())
		}
		// engine.ParseAudience devuelve errores de forma, no de infraestructura:
		// son culpa del cuerpo enviado y merecen un 400 con el motivo exacto, que
		// es lo que el panel muestra al autor.
		if _, parseErr := engine.ParseAudience(json.RawMessage(body)); parseErr != nil {
			return con.fail(c, fiber.StatusBadRequest, parseErr.Error())
		}
		return con.failFlow(c, "SetFlowAudience", bot.ID, err, "no se pudo asignar la audiencia")
	}
	if updated == nil {
		return con.fail(c, fiber.StatusNotFound, "flujo no encontrado")
	}
	msg := "audiencia asignada"
	if len(updated.Audience) == 0 {
		msg = "audiencia retirada: el flujo vuelve a atender a todos los contactos"
	}
	return con.ok(c, msg, updated)
}

// POST /bots/:botId/flows/:flowId/audience/preview — ensayo en seco: a quién
// atendería el flujo con esta condición.
//
// Acepta la condición **en el cuerpo** y no la guardada, que es el punto entero:
// sirve para ver el resultado mientras se edita, antes de asignar nada. Con el
// cuerpo vacío previsualiza la que ya tiene.
//
// Lo puede pedir cualquier miembro aunque solo owner/admin pueda asignarla: no
// expone nada nuevo —los contactos ya se ven en el panel de Datos— y un `member`
// que construye un flujo restringido necesita comprobar su condición tanto como
// quien la aprueba.
func (con *Controller) PreviewBotFlowAudience(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	raw := flow.Audience
	if body := c.Body(); len(body) > 0 {
		if !json.Valid(body) {
			return con.fail(c, fiber.StatusBadRequest, "la condición de audiencia no es JSON válido")
		}
		raw = json.RawMessage(body)
	}
	preview, err := models.PreviewAudience(c.Context(), con.Env.Postgres, bot.OrgID, raw)
	if err != nil {
		// Aquí el autor SÍ quiere ver el fallo, al revés que en el despacho, donde
		// un error se traduce a «no atiende a nadie» y se registra. Ocultarlo
		// dejaría una condición rota pareciendo una audiencia vacía.
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}
	return con.ok(c, "ok", preview)
}

// POST /bots/:botId/flows/:flowId/reset-chats — corta las conversaciones que
// siguen selladas a este flujo (owner/admin).
//
// Es la contrapartida de que la regla 1 del dispatcher ignore la audiencia: sin
// esta acción, sacar a alguien de la audiencia no surtiría efecto hasta que su
// conversación expirara, y no habría forma de forzarlo. Pide `owner/admin`
// porque interrumpe conversaciones de clientes reales a media frase.
func (con *Controller) ResetBotFlowChats(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin")
	if err != nil {
		return con.failErr(c, err)
	}
	cortadas, restantes, err := models.ResetChatsOfFlow(c.Context(), con.Env.Postgres,
		bot.ID, flow.ID, bot.OrgID, flow.Audience)
	if err != nil {
		return con.failFlow(c, "ResetChatsOfFlow", bot.ID, err, "no se pudieron cortar las conversaciones")
	}
	// `active` dice dónde están las que quedan. Sin eso, un «no había nada que
	// cortar» es cierto e inútil: el caso normal es que las conversaciones estén
	// selladas a OTRO flujo —el de reserva— y quien pulsó el botón no tiene cómo
	// saberlo.
	return con.ok(c, "conversaciones cortadas", fiber.Map{"reset": cortadas, "active": restantes})
}

// POST /bots/:botId/flows/:flowId/validate — valida el grafo sin publicar.
// Acepta un grafo en el cuerpo; si viene vacío, valida el borrador guardado.
func (con *Controller) ValidateBotFlow(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	raw := flow.Draft
	if body := c.Body(); len(body) > 0 {
		raw = json.RawMessage(body)
	}
	if problem := validateFlowDefinition(raw, flow.TriggerType); problem != "" {
		return con.ok(c, "el flujo tiene errores", fiber.Map{"valid": false, "error": problem})
	}
	templateValidation, err := models.ValidateFlowTemplates(c.Context(), con.Env.Postgres, bot.ID, raw)
	if err != nil {
		return con.ok(c, "el flujo tiene errores", fiber.Map{"valid": false, "error": err.Error()})
	}
	return con.ok(c, "el flujo es válido", fiber.Map{"valid": true, "warnings": templateValidation.Warnings})
}

// POST /bots/:botId/flows/:flowId/publish — publica el borrador.
//
// El rol exigido **depende de a quién va a atender el flujo**: un `member` puede
// publicar uno restringido a una audiencia, pero no uno que hable con todos los
// contactos del bot. Es el punto entero de este diseño — permitir dar permiso de
// publicar acotado en vez de permiso de publicar a secas—, y por eso la
// comprobación va aquí y no en un rol nuevo.
//
// Secuencia de §5.2: validar → normalizar → checksum → no-op si coincide con lo
// publicado → versión inmutable → published_version_id.
func (con *Controller) PublishBotFlow(c *fiber.Ctx) error {
	// Se carga con el conjunto amplio porque hace falta ver el flujo para saber
	// qué rol exigirle. El endurecimiento viene inmediatamente después: entre las
	// dos comprobaciones no se ha escrito nada.
	bot, flow, err := con.flowWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	if flow.ArchivedAt != nil {
		return con.fail(c, fiber.StatusConflict, models.ErrFlowArchived.Error())
	}
	expectedDraftChecksum, problem := parseExpectedDraftChecksumBody(c.Body())
	if problem != "" {
		return con.fail(c, fiber.StatusBadRequest, problem)
	}
	membership, err := con.requireOrgRole(c, bot.OrgID)
	if err != nil {
		return con.failErr(c, err)
	}
	canPublishOpen := membership.Role == "owner" || membership.Role == "admin"
	if len(flow.Audience) == 0 && !canPublishOpen {
		return con.fail(c, fiber.StatusForbidden,
			"publicar un flujo sin audiencia atiende a todos los contactos del bot y requiere un administrador. "+
				"Puedes publicarlo restringido a una audiencia, o pedir a un administrador que lo publique abierto.")
	}
	if flow.TriggerType == "schedule" {
		// Meta es la fuente de verdad: publicar fuerza una fotografía actual,
		// en vez de confiar en una caché que podría haber quedado obsoleta. La
		// validación de esa fotografía se repite después sobre el draft bloqueado,
		// dentro de PublishFlow.
		if _, err := con.syncBotTemplates(c.Context(), bot); err != nil {
			return con.fail(c, fiber.StatusBadGateway, "no se pudo confirmar las plantillas con Meta: "+err.Error())
		}
	}
	res, err := models.PublishFlow(c.Context(), con.Env.Postgres, bot.ID, flow.ID,
		expectedDraftChecksum, con.currentUserID(c), canPublishOpen)
	if err != nil {
		return con.failFlow(c, "PublishFlow", bot.ID, err, "no se pudo publicar el flujo")
	}
	if res == nil {
		return con.fail(c, fiber.StatusNotFound, "flujo no encontrado")
	}
	msg := "flujo publicado"
	if !res.Created {
		msg = "sin cambios: el grafo ya estaba publicado en esta versión"
	}
	return con.ok(c, msg, res)
}

// POST /bots/:botId/flows/:flowId/pause — deja de crear ejecuciones nuevas.
func (con *Controller) PauseBotFlow(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	updated, err := models.PauseFlow(c.Context(), con.Env.Postgres, bot.ID, flow.ID, con.currentUserID(c))
	if err != nil {
		return con.failFlow(c, "PauseFlow", bot.ID, err, "no se pudo pausar el flujo")
	}
	if updated == nil {
		return con.fail(c, fiber.StatusConflict, "el flujo está archivado")
	}
	return con.ok(c, "flujo pausado", updated)
}

// POST /bots/:botId/flows/:flowId/resume — reactiva un flujo pausado.
func (con *Controller) ResumeBotFlow(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	updated, err := models.ResumeFlow(c.Context(), con.Env.Postgres, bot.ID, flow.ID, con.currentUserID(c))
	if err != nil {
		return con.failFlow(c, "ResumeFlow", bot.ID, err, "no se pudo reactivar el flujo")
	}
	if updated == nil {
		return con.fail(c, fiber.StatusConflict, "solo se puede reactivar un flujo pausado con versión publicada")
	}
	return con.ok(c, "flujo reactivado", updated)
}

// POST /bots/:botId/flows/:flowId/archive — archiva y libera la key (§5.1).
func (con *Controller) ArchiveBotFlow(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin")
	if err != nil {
		return con.failErr(c, err)
	}
	updated, err := models.ArchiveFlow(c.Context(), con.Env.Postgres, bot.ID, flow.ID, con.currentUserID(c))
	if err != nil {
		return con.failFlow(c, "ArchiveFlow", bot.ID, err, "no se pudo archivar el flujo")
	}
	if updated == nil {
		return con.ok(c, "el flujo ya estaba archivado", flow)
	}
	return con.ok(c, "flujo archivado", updated)
}

// GET /bots/:botId/flows/:flowId/versions — historial de versiones publicadas.
func (con *Controller) ListBotFlowVersions(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	versions, err := models.ListFlowVersions(c.Context(), con.Env.Postgres, bot.ID, flow.ID)
	if err != nil {
		return con.failFlow(c, "ListFlowVersions", bot.ID, err, "no se pudieron obtener las versiones")
	}
	return con.ok(c, "ok", versions)
}

// POST /bots/:botId/flows/:flowId/versions/:versionId/restore — copia una versión
// al borrador. No publica: republicar es un acto explícito y crea una versión
// nueva (por eso flow_versions no tiene UNIQUE por checksum).
func (con *Controller) RestoreBotFlowVersion(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	if flow.ArchivedAt != nil {
		return con.fail(c, fiber.StatusConflict, models.ErrFlowArchived.Error())
	}
	expectedDraftChecksum, problem := parseExpectedDraftChecksumBody(c.Body())
	if problem != "" {
		return con.fail(c, fiber.StatusBadRequest, problem)
	}
	updated, err := models.RestoreFlowVersion(c.Context(), con.Env.Postgres, bot.ID, flow.ID,
		c.Params("versionId"), expectedDraftChecksum, con.currentUserID(c))
	if err != nil {
		return con.failFlow(c, "RestoreFlowVersion", bot.ID, err, "no se pudo restaurar la versión")
	}
	if updated == nil {
		return con.fail(c, fiber.StatusNotFound, "versión no encontrada")
	}
	return con.ok(c, "versión restaurada en el borrador", updated)
}

// validateFlowDefinition reutiliza engine.Validate (la única fuente de verdad de
// qué grafo puede ejecutar el motor). Devuelve "" si el grafo es válido.
func validateFlowDefinition(raw json.RawMessage, triggerType string) string {
	if len(raw) == 0 {
		return "el flujo está vacío"
	}
	var flow engine.Flow
	if err := json.Unmarshal(raw, &flow); err != nil {
		return "flujo inválido (no es JSON del editor)"
	}
	if triggerType != "" && flow.Trigger.Type != triggerType {
		return "el trigger del grafo (" + flow.Trigger.Type + ") no coincide con el del flujo (" + triggerType + ")"
	}
	if err := engine.Validate(&flow); err != nil {
		return err.Error()
	}
	return ""
}

func parseDraftUpdateBody(body []byte) (json.RawMessage, string, string) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil || envelope == nil {
		return nil, "", "input inválido: se esperaba {draft, expectedChecksum}"
	}
	if len(envelope) != 2 || envelope["draft"] == nil || envelope["expectedChecksum"] == nil {
		return nil, "", "input inválido: se esperaba únicamente {draft, expectedChecksum}"
	}
	var expectedChecksum string
	if err := json.Unmarshal(envelope["expectedChecksum"], &expectedChecksum); err != nil || strings.TrimSpace(expectedChecksum) == "" {
		return nil, "", "expectedChecksum es obligatorio"
	}
	draft := envelope["draft"]
	if problem := validateDraftShape(draft); problem != "" {
		return nil, "", problem
	}
	return draft, strings.TrimSpace(expectedChecksum), ""
}

// validateDraftShape admite un flujo incompleto, pero no otra raíz JSON. Evita
// que un cliente viejo o un despliegue en orden incorrecto guarde el envelope
// CAS como si fuera el propio grafo.
func validateDraftShape(raw json.RawMessage) string {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
		return "draft debe ser un objeto JSON con shape de Flow"
	}
	var id, name string
	var trigger map[string]json.RawMessage
	var nodes, edges []json.RawMessage
	if json.Unmarshal(doc["id"], &id) != nil || json.Unmarshal(doc["name"], &name) != nil ||
		json.Unmarshal(doc["trigger"], &trigger) != nil || trigger == nil ||
		json.Unmarshal(doc["nodes"], &nodes) != nil || nodes == nil ||
		json.Unmarshal(doc["edges"], &edges) != nil || edges == nil {
		return "draft debe contener id, name, trigger, nodes y edges con tipos de Flow"
	}
	return ""
}

func parseExpectedDraftChecksumBody(body []byte) (string, string) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil || envelope == nil || len(envelope) != 1 || envelope["expectedDraftChecksum"] == nil {
		return "", "input inválido: se esperaba {expectedDraftChecksum}"
	}
	var checksum string
	if err := json.Unmarshal(envelope["expectedDraftChecksum"], &checksum); err != nil || strings.TrimSpace(checksum) == "" {
		return "", "expectedDraftChecksum es obligatorio"
	}
	return strings.TrimSpace(checksum), ""
}

// failFlow traduce los errores de dominio de models/flow.go a códigos HTTP.
func (con *Controller) failFlow(c *fiber.Ctx, op, botID string, err error, fallback string) error {
	var conflict *models.DraftConflictError
	var validation *models.FlowValidationError
	switch {
	case errors.As(err, &conflict):
		return c.Status(fiber.StatusConflict).JSON(types.ErrData(conflict.Error(), conflict))
	case errors.As(err, &validation):
		return con.fail(c, fiber.StatusBadRequest, validation.Error())
	case errors.Is(err, models.ErrFlowKeyTaken), errors.Is(err, models.ErrFlowFallbackTaken):
		return con.fail(c, fiber.StatusConflict, err.Error())
	case errors.Is(err, models.ErrFlowArchived):
		return con.fail(c, fiber.StatusConflict, err.Error())
	case errors.Is(err, models.ErrFlowPublishOpenAudience):
		return con.fail(c, fiber.StatusForbidden, err.Error())
	case errors.Is(err, models.ErrFlowInvalidKey):
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}
	con.Env.Logger.Error(op, "botId", botID, "err", err.Error())
	return con.fail(c, fiber.StatusInternalServerError, fallback)
}

// messageFlowForInput elige qué flujo `message` atiende este turno.
//
//  1. Si la conversación venía a medias, sigue en **su** flujo. Cambiar de grafo
//     a mitad de un wait perdería el nodo en el que estaba y las variables ya
//     recogidas. Si ese flujo dejó de estar publicado se vuelve a despachar: el
//     operador lo pausó y pausar tiene que surtir efecto.
//  2. Si no, gana el primer flujo que (a) reconozca el mensaje por su trigger, por
//     prioridad, y (b) o no tenga audiencia, o incluya a este contacto.
//  3. Si ninguno lo reconoce, atiende el marcado como fallback, con la misma
//     condición (b) —aunque hoy la base impide que un fallback tenga audiencia—.
//
// **La regla 1 NO comprueba la audiencia, y es deliberado.** Sacar a alguien de
// su flujo a mitad de un `wait` porque dejó de cumplir la condición perdería su
// nodo y sus variables, que es justo lo que el punto 1 existe para evitar. La
// consecuencia visible: entrar o salir de la audiencia no surte efecto hasta la
// próxima conversación. Para eso está la acción de cortar conversaciones
// activas, que es una decisión del operador y no una expulsión automática.
//
// Sin versión publicada no hay grafo y el webhook cae al eco. Es deliberado:
// despublicar tiene que dejar de ejecutar, no revivir una copia paralela.
func (con *Controller) messageFlowForInput(ctx context.Context, bot *models.BotChannel, phone, stateFlowID, input string) *models.PublishedFlowRef {
	botID := bot.ID
	if stateFlowID != "" {
		ref, err := models.PublishedFlowByID(ctx, con.Env.Postgres, botID, stateFlowID)
		if err != nil {
			con.Env.Logger.Error("PublishedFlowByID", "botId", botID, "flowId", stateFlowID, "err", err.Error())
			return nil
		}
		if ref != nil {
			return ref
		}
	}
	flows, err := models.PublishedMessageFlows(ctx, con.Env.Postgres, botID)
	if err != nil {
		con.Env.Logger.Error("PublishedMessageFlows", "botId", botID, "err", err.Error())
		return nil
	}
	var fallback *models.PublishedFlowRef
	for i := range flows {
		ref := &flows[i]
		if ref.IsFallback && fallback == nil && con.audienceAdmits(ctx, bot, phone, ref) {
			fallback = ref
		}
		var graph engine.Flow
		if json.Unmarshal(ref.Definition, &graph) != nil {
			continue
		}
		// La audiencia se comprueba **después** del trigger: solo pagan consulta
		// los flujos restringidos que además reconocen el mensaje, y el bucle se
		// corta en el primero que gana. Un bot sin audiencias no consulta nada.
		if engine.TriggerMatches(graph.Trigger, input) && con.audienceAdmits(ctx, bot, phone, ref) {
			return ref
		}
	}
	return fallback
}

// audienceAdmits resuelve si este contacto entra en el flujo.
//
// La prioridad sigue siendo el único mecanismo de precedencia: un flujo con
// audiencia gana al general publicándose con `priority` menor. No se añade aquí
// ninguna regla de "el restringido manda", porque dos mecanismos de orden
// compitiendo es de donde salen los despachos impredecibles.
func (con *Controller) audienceAdmits(ctx context.Context, bot *models.BotChannel, phone string, ref *models.PublishedFlowRef) bool {
	if len(ref.Audience) == 0 {
		return true
	}
	verdict := models.ContactMatchesAudience(ctx, con.Env.Postgres, bot.OrgID, phone, ref.Audience)
	if verdict.Serves {
		return true
	}
	// Sin esta línea, un flujo restringido que no responde se depura a ciegas:
	// desde fuera es indistinguible de un trigger que no casa o de un bot caído.
	// El motivo separa el funcionamiento normal ("no cumple") de la avería que
	// alguien tiene que mirar ("no se pudo evaluar"), aunque para el contacto las
	// dos acaben igual.
	con.whatsAppLogger().Info("flujo descartado por audiencia",
		"bot", bot.ID, "flow", ref.Key, "motivo", verdict.Reason)
	return false
}
