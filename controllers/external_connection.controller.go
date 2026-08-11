package controllers

import (
	"errors"
	"regexp"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/connectors"
	"github.com/Yzx7/sacs-chatbots/connectors/meudim"
	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
)

// Conexiones a sistemas externos, desde el panel.
//
// Crear o cambiar una conexión es entregarle al bot la credencial de una tienda,
// así que exige owner o admin: un `member` puede usar el catálogo desde un flujo,
// pero no decidir a qué tienda se apunta ni con qué clave.
//
// La credencial nunca vuelve al panel. Se devuelve enmascarada —prefijo y cuatro
// últimos caracteres—, que es lo justo para reconocerla sin exponerla.

// connectionView añade a la fila lo que el panel necesita y la fila no tiene.
// El campo cifrado no aparece: el `json:"-"` del modelo lo impide, y el embebido
// lo conserva.
type connectionView struct {
	models.ExternalConnection
	CredentialMask string `json:"credentialMask"`
}

func (con *Controller) connectionViews(connections []models.ExternalConnection) []connectionView {
	views := make([]connectionView, 0, len(connections))
	for _, connection := range connections {
		view := connectionView{ExternalConnection: connection}
		if con.Env.Cipher != nil {
			if credential, err := con.Env.Cipher.Decrypt(connection.CredentialEnc); err == nil {
				view.CredentialMask = models.MaskCredential(credential)
			}
		}
		views = append(views, view)
	}
	return views
}

// ListOrgConnections — GET /orgs/:orgId/connections
func (con *Controller) ListOrgConnections(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, err := con.requireOrgRole(c, org); err != nil {
		return con.failErr(c, err)
	}
	connections, err := models.ListExternalConnections(c.Context(), con.Env.Postgres, org)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "error obteniendo las conexiones")
	}
	return con.ok(c, "ok", con.connectionViews(connections))
}

// SaveOrgConnection — POST /orgs/:orgId/connections
func (con *Controller) SaveOrgConnection(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, err := con.requireOrgRole(c, org, "owner", "admin"); err != nil {
		return con.failErr(c, err)
	}
	if con.Env.Cipher == nil {
		return con.fail(c, fiber.StatusServiceUnavailable,
			"no hay clave de cifrado configurada (TOKEN_ENC_KEY): una credencial no puede guardarse en claro")
	}
	var in struct {
		Key        string `json:"key"`
		Driver     string `json:"driver"`
		Label      string `json:"label"`
		BaseURL    string `json:"baseUrl"`
		Credential string `json:"credential"`
		Status     string `json:"status"`
	}
	if err := c.BodyParser(&in); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "conexión inválida")
	}
	in.Key = strings.TrimSpace(in.Key)
	in.Driver = strings.TrimSpace(in.Driver)
	in.BaseURL = strings.TrimSpace(in.BaseURL)
	if in.BaseURL == "" {
		in.BaseURL = connectors.DefaultBaseURL(in.Driver)
	}
	if !connectionKeyRe.MatchString(in.Key) {
		return con.fail(c, fiber.StatusBadRequest,
			"la clave técnica usa minúsculas, números y guion bajo, y empieza por letra")
	}
	if err := connectors.ValidateTarget(in.Driver, in.BaseURL); err != nil {
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}

	existing, err := models.ExternalConnectionByKey(c.Context(), con.Env.Postgres, org, in.Key)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "error consultando la conexión")
	}
	input := models.ExternalConnectionInput{
		OrgID: org, Key: in.Key, Driver: in.Driver, Label: in.Label,
		BaseURL: in.BaseURL, Status: strings.TrimSpace(in.Status),
	}
	// Una credencial vacía en una actualización conserva la guardada: es lo que
	// permite renombrar o reapuntar una conexión sin volver a pedir la clave. En
	// un alta, en cambio, no hay nada que conservar.
	if credential := strings.TrimSpace(in.Credential); credential != "" {
		if err := connectors.ValidateCredential(in.Driver, credential); err != nil {
			return con.fail(c, fiber.StatusBadRequest, err.Error())
		}
		encrypted, encErr := con.Env.Cipher.Encrypt(credential)
		if encErr != nil {
			return con.fail(c, fiber.StatusInternalServerError, "no se pudo cifrar la credencial")
		}
		input.CredentialEnc = encrypted
	} else if existing == nil {
		return con.fail(c, fiber.StatusBadRequest, "una conexión nueva requiere su credencial")
	}

	saved, err := models.SaveExternalConnection(c.Context(), con.Env.Postgres, input)
	if err != nil {
		return con.fail(c, fiber.StatusBadRequest, "no se pudo guardar la conexión")
	}
	views := con.connectionViews([]models.ExternalConnection{*saved})
	if existing == nil {
		return c.Status(fiber.StatusCreated).JSON(types.OK("conexión creada", views[0]))
	}
	return con.ok(c, "conexión actualizada", views[0])
}

// TestOrgConnection — POST /orgs/:orgId/connections/:key/test
//
// Es la única forma de distinguir una credencial revocada de una tienda caída
// sin pasar por una conversación real. Solo lee: consulta los datos públicos de
// la tienda, nunca crea un pedido.
func (con *Controller) TestOrgConnection(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, err := con.requireOrgRole(c, org, "owner", "admin", "member"); err != nil {
		return con.failErr(c, err)
	}
	if con.Env.Cipher == nil {
		return con.fail(c, fiber.StatusServiceUnavailable, "no hay clave de cifrado configurada (TOKEN_ENC_KEY)")
	}
	connection, err := models.ExternalConnectionByKey(c.Context(), con.Env.Postgres, org, c.Params("key"))
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "error consultando la conexión")
	}
	if connection == nil {
		return con.fail(c, fiber.StatusNotFound, "la conexión no existe")
	}
	credential, err := con.Env.Cipher.Decrypt(connection.CredentialEnc)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo descifrar la credencial")
	}
	client, err := meudim.New(connection.BaseURL, credential, nil)
	if err != nil {
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}

	store, meta, callErr := client.Store(c.Context())
	// El resultado se anota siempre: es lo que después explica en la lista por qué
	// un flujo dejó de encontrar productos.
	if recErr := models.RecordExternalConnectionResult(c.Context(), con.Env.Postgres, connection.ID, callErr); recErr != nil {
		con.Env.Logger.Warn("conexión: no se pudo anotar el resultado", "connection", connection.Key, "err", recErr.Error())
	}
	if callErr != nil {
		return con.fail(c, fiber.StatusBadGateway, connectionFailureMessage(callErr))
	}
	return con.ok(c, "conexión verificada", fiber.Map{
		"storeId":        store.ID,
		"storeName":      store.Name,
		"domain":         store.Domain,
		"currency":       store.Settings.Currency,
		"rateRemaining":  meta.RateLimitRemaining,
		"credentialMask": models.MaskCredential(credential),
	})
}

// DeleteOrgConnection — DELETE /orgs/:orgId/connections/:key
func (con *Controller) DeleteOrgConnection(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, err := con.requireOrgRole(c, org, "owner", "admin"); err != nil {
		return con.failErr(c, err)
	}
	deleted, err := models.DeleteExternalConnection(c.Context(), con.Env.Postgres, org, c.Params("key"))
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo eliminar la conexión")
	}
	if !deleted {
		return con.fail(c, fiber.StatusNotFound, "la conexión no existe")
	}
	return con.ok(c, "conexión eliminada", nil)
}

// connectionFailureMessage traduce el fallo a la causa que hay que atender. Un
// «502» a secas no le dice a nadie si rotar la clave o esperar a que la tienda
// vuelva.
func connectionFailureMessage(err error) string {
	var apiErr *meudim.Error
	if !errors.As(err, &apiErr) {
		return err.Error()
	}
	switch {
	case apiErr.Unauthorized():
		return "la clave es inválida, fue revocada o pertenece a otra tienda"
	case apiErr.RateLimited():
		return "la tienda alcanzó su límite de peticiones; inténtalo en un minuto"
	case apiErr.Unreachable:
		return "no hubo respuesta de la tienda"
	default:
		return apiErr.Message
	}
}

// connectionKeyRe espeja el CHECK de la tabla. Se valida aquí para devolver un
// 400 con explicación en vez de un error de restricción de PostgreSQL.
var connectionKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
