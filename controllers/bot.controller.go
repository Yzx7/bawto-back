package controllers

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/channels/whatsapp"
	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
)

// Ruta del panel que recibe la vuelta del Embedded Signup. Espejo de
// WA_REDIRECT_PATH en frontend/components/dashboard/embedded-signup.tsx; si una
// cambia, la otra tambien, porque Meta exige que el redirect_uri del intercambio
// sea identico al del dialogo.
const embeddedRedirectPath = "/oauth/whatsapp"

// Cuanto vive el sobre de seleccion. Es el tiempo de elegir un numero en una
// lista, no el de completar un flujo entero.
const embeddedSelectionTTL = 10 * time.Minute

// Ventana para creer que un PARTNER_ADDED pertenece a este signup. Generosa
// porque el cliente puede demorarse dentro del dialogo de Meta, y no importa
// que lo sea: solo desempata entre cuentas que el token ya autoriza.
const embeddedLinkWindow = 30 * time.Minute

// preferRecentlyLinked se queda con las cuentas del token que Meta acaba de
// vincular, conservando el orden del webhook —la mas reciente primero—.
//
// Si el cruce queda vacio devuelve la lista original: el webhook es una pista
// para no preguntar de mas, no una autoridad. Nunca añade una cuenta que el
// token no alcance, porque eso conectaria el bot a algo que el cliente no
// autorizo en este flujo.
func preferRecentlyLinked(fromToken, recentlyLinked []string) []string {
	if len(fromToken) < 2 || len(recentlyLinked) == 0 {
		return fromToken
	}
	allowed := make(map[string]bool, len(fromToken))
	for _, waba := range fromToken {
		allowed[waba] = true
	}
	var out []string
	for _, waba := range recentlyLinked {
		if allowed[waba] {
			out = append(out, waba)
		}
	}
	if len(out) == 0 {
		return fromToken
	}
	return out
}

// embeddedSelection viaja al navegador **cifrada**: lleva el access token del
// cliente, que no debe salir en claro ni siquiera hacia su propio panel.
type embeddedSelection struct {
	Token string `json:"t"`
	BotID string `json:"b"`
	Exp   int64  `json:"e"`
}

// sealEmbeddedSelection guarda el token ya intercambiado en un sobre que solo
// este servidor puede abrir. Evita tener que inventar estado de sesion para un
// paso que dura segundos, y evita repetir el intercambio: el code de Meta es de
// un solo uso.
func (con *Controller) sealEmbeddedSelection(token, botID string) (string, error) {
	payload, err := json.Marshal(embeddedSelection{
		Token: token,
		BotID: botID,
		Exp:   time.Now().Add(embeddedSelectionTTL).Unix(),
	})
	if err != nil {
		return "", err
	}
	enc, err := con.Env.Cipher.Encrypt(string(payload))
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(enc), nil
}

// openEmbeddedSelection abre el sobre y comprueba que sigue siendo valido para
// **este** bot: un sobre de otro bot no debe poder conectar este.
func (con *Controller) openEmbeddedSelection(sealed, botID string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return "", errors.New("seleccion invalida")
	}
	plain, err := con.Env.Cipher.Decrypt(raw)
	if err != nil {
		return "", errors.New("seleccion invalida")
	}
	var sel embeddedSelection
	if err := json.Unmarshal([]byte(plain), &sel); err != nil {
		return "", errors.New("seleccion invalida")
	}
	if sel.BotID != botID {
		return "", errors.New("seleccion invalida")
	}
	if time.Now().Unix() > sel.Exp {
		return "", errors.New("la seleccion caduco; repite la conexion")
	}
	return sel.Token, nil
}

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
		RedirectURI   string `json:"redirectUri"`
		// Segunda vuelta: el cliente elige cuenta y numero, y devuelve el
		// sobre cifrado que le dimos con el token ya intercambiado.
		SelectionToken string `json:"selectionToken"`
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
	b.RedirectURI = strings.TrimSpace(b.RedirectURI)
	b.SelectionToken = strings.TrimSpace(b.SelectionToken)
	// Solo se acepta la ruta de vuelta del propio panel. El valor no redirige a
	// nadie aqui —solo tiene que coincidir con el del dialogo—, pero el panel
	// vive en varios dominios y no hay una lista fija que comparar.
	if b.RedirectURI != "" {
		u, err := url.Parse(b.RedirectURI)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != embeddedRedirectPath {
			return con.fail(c, fiber.StatusBadRequest, "redirectUri invalido")
		}
	}
	// Solo el code es obligatorio: los ids llegan al panel por postMessage y en un
	// navegador movil no llegan nunca. Si faltan se deducen del token mas abajo,
	// porque descartarlos aqui tira una conexion que en Meta ya quedo hecha.
	if b.Code == "" && b.SelectionToken == "" {
		return con.fail(c, fiber.StatusBadRequest, "code es obligatorio")
	}
	if b.Mode != "cloud" && b.Mode != "coexistence" {
		return con.fail(c, fiber.StatusBadRequest, "modo de conexion invalido")
	}
	// El PIN se exige justo antes de registrar el numero, no aqui: el code de
	// Meta vive 30 segundos y rechazar la peticion por un PIN ausente lo tira
	// sin haberlo intercambiado siquiera.

	// El token sale del code la primera vez; en la segunda vuelta viene dentro
	// del sobre cifrado, porque el code de Meta es de un solo uso y ya se gasto.
	var token string
	if b.SelectionToken != "" {
		token, err = con.openEmbeddedSelection(b.SelectionToken, bot.ID)
		if err != nil {
			return con.fail(c, fiber.StatusBadRequest, err.Error())
		}
	} else {
		token, err = whatsapp.ExchangeCode(c.Context(), cfg.WhatsAppAPIBase, cfg.WhatsAppAPIVersion,
			cfg.FacebookAppID, cfg.WhatsAppAppSecret, b.Code, b.RedirectURI, nil)
		if err != nil {
			con.Env.Logger.Error("embedded exchange", "err", err.Error())
			return con.fail(c, fiber.StatusBadGateway, "no se pudo intercambiar el código con Meta")
		}
	}

	// Descubrimiento: el token de ES sabe a que WABA pertenece y que numeros
	// tiene. Solo se consulta lo que el panel no pudo entregar.
	displayPhone := ""
	if b.WabaID == "" || b.PhoneNumberID == "" {
		wabas := []string{b.WabaID}
		if b.WabaID == "" {
			wabas, err = whatsapp.DiscoverWABAs(c.Context(), cfg.WhatsAppAPIBase, cfg.WhatsAppAPIVersion,
				cfg.FacebookAppID, cfg.WhatsAppAppSecret, token, nil)
			if err != nil {
				con.Env.Logger.Error("embedded discover wabas", "err", err.Error())
				return con.fail(c, fiber.StatusBadGateway, "no se pudo identificar la cuenta de WhatsApp con Meta")
			}
		}
		if len(wabas) == 0 {
			return con.fail(c, fiber.StatusBadRequest, "el flujo termino sin una cuenta de WhatsApp; reintenta y agrega el numero")
		}
		// El token dice a que cuentas alcanza el permiso, pero no a cual acaba de
		// entrar el cliente. Eso lo sabe el webhook, que llega al servidor y no
		// depende de que el navegador pueda devolver nada.
		if len(wabas) > 1 {
			linked, err := models.RecentlyLinkedWABAs(c.Context(), con.Env.Postgres, embeddedLinkWindow)
			if err != nil {
				con.Env.Logger.Error("embedded wabas recientes", "err", err.Error())
			} else if narrowed := preferRecentlyLinked(wabas, linked); len(narrowed) < len(wabas) {
				con.Env.Logger.Info("embedded waba por webhook",
					"bot", bot.ID, "token", strings.Join(wabas, ","), "elegidas", strings.Join(narrowed, ","))
				wabas = narrowed
			}
		}

		accounts := make([]fiber.Map, 0, len(wabas))
		total, seen := 0, 0
		var onlyWaba, onlyPhone, onlyDisplay string
		for _, waba := range wabas {
			numbers, err := whatsapp.ListPhoneNumbers(c.Context(), cfg.WhatsAppAPIBase, cfg.WhatsAppAPIVersion, waba, token, nil)
			if err != nil {
				con.Env.Logger.Error("embedded list phone numbers", "waba", waba, "err", err.Error())
				return con.fail(c, fiber.StatusBadGateway, "no se pudieron leer los numeros de la cuenta de WhatsApp")
			}
			seen += len(numbers)
			items := make([]fiber.Map, 0, len(numbers))
			for _, n := range numbers {
				// Un numero ya tomado se ofrece marcado, no se esconde: saber que
				// existe y por que no sirve evita repetir el flujo entero.
				takenBy := ""
				if owner, err := models.GetBotByChannel(c.Context(), con.Env.Postgres, "wsp", n.ID); err == nil && owner != nil && owner.ID != bot.ID {
					takenBy = owner.ID
				}
				items = append(items, fiber.Map{
					"id":                 n.ID,
					"displayPhoneNumber": n.DisplayPhoneNumber,
					"verifiedName":       n.VerifiedName,
					"takenByBotId":       takenBy,
				})
				if takenBy == "" {
					total++
					onlyWaba, onlyPhone, onlyDisplay = waba, n.ID, n.DisplayPhoneNumber
				}
			}
			accounts = append(accounts, fiber.Map{"wabaId": waba, "numbers": items})
		}

		// Con una sola opcion libre no hay nada que preguntar.
		if total == 1 {
			b.WabaID, b.PhoneNumberID, displayPhone = onlyWaba, onlyPhone, onlyDisplay
		} else {
			if total == 0 {
				// "Sin numeros" y "todos ocupados" son problemas distintos y se
				// arreglan de forma distinta; darles el mismo mensaje mando al
				// dueño a buscar donde no era.
				if seen == 0 {
					con.Env.Logger.Info("embedded sin numeros", "bot", bot.ID, "wabas", strings.Join(wabas, ","))
					return con.fail(c, fiber.StatusConflict,
						"la cuenta de WhatsApp todavia no expone ningun numero; en Coexistence puede tardar unos segundos tras terminar en Meta, reintenta")
				}
				return con.fail(c, fiber.StatusConflict, "todos los numeros de esas cuentas ya estan conectados a otro bot")
			}
			// Elegir por el cliente conectaria el bot al numero equivocado. El
			// sobre lleva el token ya intercambiado para que la eleccion no
			// dependa de un code gastado.
			sealed, err := con.sealEmbeddedSelection(token, bot.ID)
			if err != nil {
				return con.fail(c, fiber.StatusInternalServerError, "no se pudo preparar la seleccion")
			}
			// Que cuentas ve el token no es evidente desde fuera: depende del
			// portfolio de la cuenta de Meta que hizo el signup, no de la
			// organizacion de Bawto. Sin esto, un "hay varias" no dice cuales.
			con.Env.Logger.Info("embedded seleccion pendiente",
				"bot", bot.ID, "wabas", strings.Join(wabas, ","), "libres", total)
			return con.ok(c, "elige el numero de WhatsApp", fiber.Map{
				"needsSelection": true,
				"selectionToken": sealed,
				"accounts":       accounts,
			})
		}
	}

	// Tras una eleccion manual el numero visible no se conoce; se busca para no
	// dejar bots.phone vacio, que es lo que ve el usuario en el panel.
	if displayPhone == "" && b.WabaID != "" {
		if numbers, err := whatsapp.ListPhoneNumbers(c.Context(), cfg.WhatsAppAPIBase, cfg.WhatsAppAPIVersion, b.WabaID, token, nil); err == nil {
			for _, n := range numbers {
				if n.ID == b.PhoneNumberID {
					displayPhone = n.DisplayPhoneNumber
					break
				}
			}
		}
	}

	// Dos bots con el mismo phone_number_id rompen el webhook: GetBotByChannel
	// exige exactamente una fila y falla al enrutar el mensaje entrante.
	if owner, err := models.GetBotByChannel(c.Context(), con.Env.Postgres, "wsp", b.PhoneNumberID); err == nil && owner != nil && owner.ID != bot.ID {
		return con.fail(c, fiber.StatusConflict, "ese numero ya esta conectado a otro bot; desconectalo alli antes de usarlo aqui")
	}

	if b.Mode == "cloud" {
		if len(b.Pin) != 6 || strings.Trim(b.Pin, "0123456789") != "" {
			return con.fail(c, fiber.StatusBadRequest, "el PIN debe tener 6 digitos")
		}
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

	// Coexistence: pedir contactos e historial de la app movil. Meta da 24 h
	// desde el onboarding y **solo admite una peticion por tipo**; pasado el
	// plazo el cliente tiene que desconectarse y repetir el flujo entero. Va
	// despues de guardar a proposito: si esto falla el canal ya quedo conectado
	// y se puede reintentar, mientras que perder el token no se arregla.
	warning := ""
	if b.Mode == "coexistence" {
		for _, syncType := range []string{whatsapp.SyncContacts, whatsapp.SyncHistory} {
			if err := whatsapp.StartSMBAppDataSync(c.Context(), cfg.WhatsAppAPIBase, cfg.WhatsAppAPIVersion,
				b.PhoneNumberID, token, syncType, nil); err != nil {
				con.Env.Logger.Error("embedded smb sync", "bot", bot.ID, "tipo", syncType, "err", err.Error())
				warning = "el canal quedo conectado, pero no se pudo iniciar la sincronizacion con la app movil; hay 24 h para reintentarlo"
			}
		}
		if status, err := whatsapp.GetPhoneNumberStatus(c.Context(), cfg.WhatsAppAPIBase, cfg.WhatsAppAPIVersion,
			b.PhoneNumberID, token, nil); err == nil {
			con.Env.Logger.Info("embedded coexistence", "bot", bot.ID,
				"is_on_biz_app", status.IsOnBizApp, "platform_type", status.PlatformType)
		}
	}
	if warning != "" {
		return con.ok(c, warning, fiber.Map{"syncWarning": true})
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
