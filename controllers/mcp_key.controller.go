package controllers

import (
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/models"
)

// Keys del servicio MCP de autoría. Cuelgan de `/orgs/:orgId` y van protegidas
// por el JWT del panel como el resto: quien reparte credenciales es una persona
// con sesión, no otra máquina.
//
// Crear y revocar exigen owner/admin. Una key es una llave de escritura sobre
// los flujos de la organización, así que repartirla no es una tarea de edición
// como renombrar un flujo: un `member` puede construir su flujo entero sin
// necesitar emitir credenciales para terceros.

// mcpKeyCreated es la única respuesta que lleva la key en claro. Sale del
// servidor una vez y no se puede volver a consultar: en la base solo queda su
// hash.
type mcpKeyCreated struct {
	Key    *models.MCPKey `json:"key"`
	Secret string         `json:"secret"`
	Notice string         `json:"notice"`
}

// POST /orgs/:orgId/mcp-keys — emite una key (owner/admin).
func (con *Controller) CreateOrgMCPKey(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID, "owner", "admin"); err != nil {
		return con.failErr(c, err)
	}
	var body struct {
		Name string `json:"name"`
		// FlowIDs vacío da acceso a todos los flujos de la organización. Es el
		// valor por defecto porque una lista ausente no puede significar «ninguno»:
		// una key que no alcanza nada no tiene para qué existir.
		FlowIDs []string `json:"flowIds"`
		// ExpiresInDays es opcional; sin él la key vale hasta que se revoque.
		ExpiresInDays int `json:"expiresInDays"`
	}
	if err := c.BodyParser(&body); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido")
	}
	if strings.TrimSpace(body.Name) == "" {
		return con.fail(c, fiber.StatusBadRequest, "el nombre es obligatorio")
	}
	if body.ExpiresInDays < 0 || body.ExpiresInDays > 3650 {
		return con.fail(c, fiber.StatusBadRequest, "la caducidad va de 1 a 3650 días")
	}
	input := models.NewMCPKey{
		Name: body.Name, FlowIDs: body.FlowIDs, CreatedBy: con.currentUserID(c),
	}
	if body.ExpiresInDays > 0 {
		expires := time.Now().UTC().AddDate(0, 0, body.ExpiresInDays)
		input.ExpiresAt = &expires
	}
	created, secret, err := models.CreateMCPKey(c.Context(), con.Env.Postgres, orgID, input)
	if err != nil {
		if errors.Is(err, models.ErrMCPKeyInvalidScope) {
			return con.fail(c, fiber.StatusBadRequest, err.Error())
		}
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo crear la key")
	}
	return con.ok(c, "key creada", mcpKeyCreated{
		Key: created, Secret: secret,
		Notice: "Copia la key ahora: no se vuelve a mostrar. Si se pierde, revócala y crea otra.",
	})
}

// GET /orgs/:orgId/mcp-keys — lista las keys sin su secreto (owner/admin/member).
//
// Un `member` puede verlas porque saber qué credenciales existen y cuándo se
// usaron por última vez es información de seguridad, no un privilegio: la lista
// no incluye nada con lo que autenticarse.
func (con *Controller) ListOrgMCPKeys(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID, "owner", "admin", "member"); err != nil {
		return con.failErr(c, err)
	}
	keys, err := models.ListMCPKeys(c.Context(), con.Env.Postgres, orgID)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudieron listar las keys")
	}
	return con.ok(c, "ok", keys)
}

// DELETE /orgs/:orgId/mcp-keys/:keyId — revoca (owner/admin).
//
// Revoca y no borra: la fila es el registro de qué se repartió y cuándo dejó de
// valer. Borrarla dejaría un hueco justo donde hace falta una auditoría.
func (con *Controller) RevokeOrgMCPKey(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID, "owner", "admin"); err != nil {
		return con.failErr(c, err)
	}
	revoked, err := models.RevokeMCPKey(c.Context(), con.Env.Postgres, orgID,
		c.Params("keyId"), con.currentUserID(c))
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo revocar la key")
	}
	if revoked == nil {
		return con.fail(c, fiber.StatusNotFound, "key no encontrada")
	}
	return con.ok(c, "key revocada", revoked)
}
