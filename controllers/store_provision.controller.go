package controllers

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/connectors"
	"github.com/Yzx7/sacs-chatbots/connectors/meudim"
	"github.com/Yzx7/sacs-chatbots/connectors/meudprov"
	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
)

// Crear la tienda de una organización sin salir del panel ni volver a iniciar
// sesión (PLAN-TIENDA-NATIVA-DESDE-BAWTO.md, contrato en GUIA-CONEXION-MEUD.md).
//
// **La credencial de aprovisionamiento no puede salir al navegador.** Vale para
// todas las tiendas de todos los clientes, así que el panel llama aquí y solo
// este backend habla con MEUD. Por eso existe este endpoint y no una llamada
// directa desde el asistente.
//
// A partir de la `sk_` devuelta el camino es **idéntico** al de quien pega su
// clave a mano: la misma conexión `meudim`, el mismo driver y la misma prueba.
// Eso no es casualidad, es lo que se buscó al construirlo así.

// storeConnectionKey es la clave técnica de la conexión a la tienda. El grafo la
// nombra literalmente, así que vale igual para todas las organizaciones.
const storeConnectionKey = "meudim"

// meudProvisionClient construye el cliente de aprovisionamiento, o explica por
// qué no se puede. El 503 distingue «no está configurado» de «falló»: lo primero
// se arregla en el servidor y no tiene sentido reintentarlo.
func (con *Controller) meudProvisionClient() (*meudprov.Client, error) {
	cfg := con.Env.Config
	if ready, reason := cfg.MeudProvisionReadiness(); !ready {
		return nil, fiber.NewError(fiber.StatusServiceUnavailable,
			"la creación automática de tiendas no está configurada en este servidor ("+reason+")")
	}
	client, err := meudprov.New(cfg.MeudProvisionURL, cfg.MeudProvisionKey, nil)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, err.Error())
	}
	return client, nil
}

// ProvisionOrgStore — POST /orgs/:orgId/store
//
// Crea la tienda en MEUD y guarda su clave como la conexión `meudim` de la
// organización. Owner o admin: entregarle al bot una tienda es la misma decisión
// que entregarle una credencial.
func (con *Controller) ProvisionOrgStore(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, err := con.requireOrgRole(c, org, "owner", "admin"); err != nil {
		return con.failErr(c, err)
	}
	if con.Env.Cipher == nil {
		return con.fail(c, fiber.StatusServiceUnavailable,
			"no hay clave de cifrado configurada (TOKEN_ENC_KEY): una credencial no puede guardarse en claro")
	}
	client, err := con.meudProvisionClient()
	if err != nil {
		return con.failErr(c, err)
	}

	// Una conexión ya puesta no se pisa. Si la organización pegó su propia clave,
	// crear otra tienda dejaría un catálogo vacío detrás del mismo bot y la
	// primera consulta real respondería «no tenemos productos».
	existing, err := models.ExternalConnectionByKey(c.Context(), con.Env.Postgres, org, storeConnectionKey)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "error consultando la conexión")
	}
	if existing != nil {
		return con.fail(c, fiber.StatusConflict,
			"esta organización ya tiene una tienda conectada; elimínala antes de crear otra")
	}

	var in struct {
		Name string `json:"name"`
	}
	// El cuerpo es opcional: sin nombre se usa el de la organización, que es lo
	// que el asistente propone de todas formas.
	_ = c.BodyParser(&in)
	name := strings.TrimSpace(in.Name)
	if name == "" {
		organization, orgErr := models.GetOrganization(c.Context(), con.Env.Postgres, org)
		if orgErr != nil || organization == nil {
			return con.fail(c, fiber.StatusNotFound, "la organización no existe")
		}
		name = strings.TrimSpace(organization.Name)
	}
	if name == "" {
		return con.fail(c, fiber.StatusBadRequest, "la tienda necesita un nombre")
	}

	// La clave de idempotencia es la organización que pide la tienda: estable
	// entre reintentos y única por organización. Es lo único que impide que un
	// timeout o un doble clic dejen dos tiendas.
	store, err := client.CreateStore(c.Context(), name, "bawto:org:"+org)
	if err != nil {
		status, message := provisionFailure(err)
		return con.fail(c, status, message)
	}

	if strings.TrimSpace(store.SK) == "" {
		// MEUD respondió que ya existía, pero aquí no quedó la clave: se perdió
		// una respuesta anterior. No hay reemisión automática a propósito —emitir
		// otra revocaría la que quizá esté funcionando—, así que esto se resuelve
		// a mano y conviene decir exactamente qué tienda es.
		return con.fail(c, fiber.StatusConflict, fmt.Sprintf(
			"MEUD ya había creado la tienda %d para esta organización, pero su clave solo se entrega una vez "+
				"y aquí no quedó guardada: hay que emitir una nueva desde el panel de MEUD y pegarla como conexión",
			store.ID))
	}

	encrypted, err := con.Env.Cipher.Encrypt(store.SK)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo cifrar la credencial de la tienda")
	}
	provisioned := strconv.FormatInt(store.ID, 10)
	saved, err := models.SaveExternalConnection(c.Context(), con.Env.Postgres, models.ExternalConnectionInput{
		OrgID:         org,
		Key:           storeConnectionKey,
		Driver:        connectors.DriverMeudim,
		Label:         name,
		BaseURL:       connectors.DefaultBaseURL(connectors.DriverMeudim),
		CredentialEnc: encrypted,
		Status:        "active",
		ProvisionedID: &provisioned,
	})
	if err != nil {
		// La tienda quedó creada y su clave se pierde aquí. Se dice con el id
		// delante: es lo único que permite recuperarla desde el panel de MEUD.
		con.Env.Logger.Error("aprovisionamiento: la tienda se creó y su clave no se pudo guardar",
			"org", org, "store", store.ID, "err", err.Error())
		return con.fail(c, fiber.StatusInternalServerError, fmt.Sprintf(
			"la tienda %d se creó en MEUD pero no se pudo guardar su clave aquí; hay que emitir una nueva desde su panel",
			store.ID))
	}

	// Probar es lo mismo que hace el camino manual, con una diferencia que
	// importa: aquí un fallo **no** borra la conexión. La `sk_` no se puede
	// volver a pedir, así que borrarla dejaría la tienda creada e inalcanzable.
	// Se anota el error y se devuelve como aviso.
	warning := ""
	if credential, decErr := con.Env.Cipher.Decrypt(saved.CredentialEnc); decErr == nil {
		if api, apiErr := meudim.New(saved.BaseURL, credential, nil); apiErr == nil {
			_, _, callErr := api.Store(c.Context())
			if recErr := models.RecordExternalConnectionResult(c.Context(), con.Env.Postgres, saved.ID, callErr); recErr != nil {
				con.Env.Logger.Warn("conexión: no se pudo anotar el resultado", "connection", saved.Key, "err", recErr.Error())
			}
			if callErr != nil {
				warning = "la tienda se creó, pero todavía no responde: " + connectionFailureMessage(callErr)
			}
		}
	}

	views := con.connectionViews([]models.ExternalConnection{*saved})
	return c.Status(fiber.StatusCreated).JSON(types.OK("tienda creada", fiber.Map{
		"connection": views[0],
		"storeId":    store.ID,
		"storeName":  name,
		"warning":    warning,
	}))
}

// GrantOrgStoreAdmin — POST /orgs/:orgId/store/admins
//
// Da acceso al cliente a su tienda en meud.im, con rol `admin`. Es lo que
// convierte «Bawto la administra por mí» en «además puedo entrar yo».
//
// Solo funciona sobre tiendas que **abrimos nosotros** (`provisioned_id`). La
// credencial de aprovisionamiento vale para todas las tiendas de MEUD, así que
// sin esa comprobación bastaría con pegar la `sk_` de una tienda ajena para
// conseguir una cuenta en su panel: convertiríamos tener una clave en tener
// acceso permanente.
func (con *Controller) GrantOrgStoreAdmin(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, err := con.requireOrgRole(c, org, "owner", "admin"); err != nil {
		return con.failErr(c, err)
	}
	client, err := con.meudProvisionClient()
	if err != nil {
		return con.failErr(c, err)
	}
	var in struct {
		Email string `json:"email"`
	}
	if err := c.BodyParser(&in); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "cuerpo inválido")
	}
	email := strings.TrimSpace(in.Email)
	if email == "" || !strings.Contains(email, "@") {
		return con.fail(c, fiber.StatusBadRequest, "hace falta el correo de la cuenta de Meudim")
	}

	connection, err := models.ExternalConnectionByKey(c.Context(), con.Env.Postgres, org, storeConnectionKey)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "error consultando la conexión")
	}
	if connection == nil {
		return con.fail(c, fiber.StatusNotFound, "esta organización no tiene una tienda conectada")
	}
	if connection.ProvisionedID == nil || strings.TrimSpace(*connection.ProvisionedID) == "" {
		return con.fail(c, fiber.StatusBadRequest,
			"esta tienda no la creó Bawto: sus administradores se gestionan desde el panel de Meudim")
	}
	storeID, err := strconv.ParseInt(strings.TrimSpace(*connection.ProvisionedID), 10, 64)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "el identificador de la tienda guardado no es válido")
	}

	already, err := client.AddAdmin(c.Context(), storeID, email)
	if err != nil {
		status, message := provisionFailure(err)
		return con.fail(c, status, message)
	}
	if already {
		return con.ok(c, "esa cuenta ya administra la tienda", fiber.Map{"storeId": storeID, "already": true})
	}
	return con.ok(c, "cuenta añadida como administradora de la tienda",
		fiber.Map{"storeId": storeID, "already": false})
}

// provisionFailure traduce el fallo al código y al texto que corresponde.
//
// Los mensajes de MEUD se conservan cuando son para una persona —el 404 de
// admins es literalmente «pídele que cree su cuenta primero»— y se sustituyen
// cuando delatan configuración del servidor, que no le dice nada a quien está
// mirando el asistente.
func provisionFailure(err error) (int, string) {
	var apiErr *meudprov.Error
	if !errors.As(err, &apiErr) {
		return fiber.StatusBadGateway, err.Error()
	}
	switch {
	case apiErr.Unreachable:
		return fiber.StatusBadGateway, "no hubo respuesta de Meudim; inténtalo de nuevo en un momento"
	case apiErr.Unauthorized():
		return fiber.StatusServiceUnavailable,
			"la clave de aprovisionamiento no es válida; hay que revisarla en el servidor"
	case apiErr.Unavailable():
		return fiber.StatusServiceUnavailable, "Meudim no tiene el aprovisionamiento configurado"
	case apiErr.NotFound():
		// Es el mensaje que hay que mostrarle al cliente tal cual.
		return fiber.StatusNotFound, apiErr.Message
	case apiErr.StatusCode >= 500:
		return fiber.StatusBadGateway, "Meudim falló al crear la tienda; puede reintentarse"
	default:
		return fiber.StatusBadGateway, apiErr.Message
	}
}
