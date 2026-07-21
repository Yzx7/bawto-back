package controllers

import (
	"errors"
	"slices"
	"strings"

	"github.com/gofiber/fiber/v2"

	authmw "github.com/Yzx7/sacs-chatbots/middlewares/auth"
	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
)

var validMemberRoles = []string{"admin", "member", "viewer"}

// GET /orgs — organizaciones del usuario autenticado.
func (con *Controller) GetMyOrganizations(c *fiber.Ctx) error {
	claims, _ := authmw.Current(c)
	orgs, err := models.GetUserOrganizations(c.Context(), con.Env.Postgres, claims.UserID)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "error obteniendo organizaciones")
	}
	return con.ok(c, "ok", orgs)
}

// POST /orgs — crea org + membership owner (transacción).
func (con *Controller) CreateOrganization(c *fiber.Ctx) error {
	var b struct {
		Name string  `json:"name"`
		RUC  *string `json:"ruc"`
		Cel  *string `json:"cel"`
	}
	if err := c.BodyParser(&b); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido")
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" {
		return con.fail(c, fiber.StatusBadRequest, "el nombre es obligatorio")
	}

	claims, _ := authmw.Current(c)
	org, err := models.CreateOrganization(c.Context(), con.Env.Postgres, claims.UserID, b.Name, b.RUC, b.Cel)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo crear la organización")
	}
	return c.Status(fiber.StatusCreated).JSON(types.OK("organización creada", org))
}

// PUT /orgs/:orgId — actualiza (owner/admin).
func (con *Controller) UpdateOrganization(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID, "owner", "admin"); err != nil {
		return con.failErr(c, err)
	}

	var b struct {
		Name string  `json:"name"`
		RUC  *string `json:"ruc"`
		Cel  *string `json:"cel"`
	}
	if err := c.BodyParser(&b); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido")
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" {
		return con.fail(c, fiber.StatusBadRequest, "el nombre es obligatorio")
	}

	org, err := models.UpdateOrganization(c.Context(), con.Env.Postgres, orgID, b.Name, b.RUC, b.Cel)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo actualizar")
	}
	if org == nil {
		return con.fail(c, fiber.StatusNotFound, "organización no encontrada")
	}
	return con.ok(c, "organización actualizada", org)
}

// DELETE /orgs/:orgId — elimina (owner).
func (con *Controller) DeleteOrganization(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID, "owner"); err != nil {
		return con.failErr(c, err)
	}
	if err := models.DeleteOrganization(c.Context(), con.Env.Postgres, orgID); err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo eliminar")
	}
	return con.ok(c, "organización eliminada", nil)
}

// GET /orgs/:orgId/members — lista miembros (cualquier miembro).
func (con *Controller) GetOrgMembers(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID); err != nil {
		return con.failErr(c, err)
	}
	members, err := models.GetOrgMemberships(c.Context(), con.Env.Postgres, orgID)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "error obteniendo miembros")
	}
	return con.ok(c, "ok", members)
}

// POST /orgs/:orgId/members — agrega miembro por email (owner/admin).
func (con *Controller) AddOrgMember(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID, "owner", "admin"); err != nil {
		return con.failErr(c, err)
	}

	var b struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := c.BodyParser(&b); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido")
	}
	b.Email = strings.TrimSpace(strings.ToLower(b.Email))
	b.Role = strings.TrimSpace(b.Role)
	if b.Email == "" {
		return con.fail(c, fiber.StatusBadRequest, "el email es obligatorio")
	}
	if b.Role == "" {
		b.Role = "member"
	}
	if !slices.Contains(validMemberRoles, b.Role) {
		return con.fail(c, fiber.StatusBadRequest, "rol inválido")
	}

	userID, err := models.FindUserIDByEmail(c.Context(), con.Env.Postgres, b.Email)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "error buscando usuario")
	}
	if userID == "" {
		return con.fail(c, fiber.StatusNotFound, "el usuario debe iniciar sesión al menos una vez antes de ser agregado")
	}

	m, err := models.CreateMembership(c.Context(), con.Env.Postgres, userID, orgID, b.Role)
	if errors.Is(err, models.ErrDuplicateMember) {
		return con.fail(c, fiber.StatusConflict, "el usuario ya es miembro")
	}
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo agregar el miembro")
	}
	return c.Status(fiber.StatusCreated).JSON(types.OK("miembro agregado", m))
}

// PUT /orgs/:orgId/members/:memberId/role — cambia rol (owner/admin).
func (con *Controller) UpdateMemberRole(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID, "owner", "admin"); err != nil {
		return con.failErr(c, err)
	}

	var b struct {
		Role string `json:"role"`
	}
	if err := c.BodyParser(&b); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "input inválido")
	}
	b.Role = strings.TrimSpace(b.Role)
	if !slices.Contains(validMemberRoles, b.Role) {
		return con.fail(c, fiber.StatusBadRequest, "rol inválido")
	}

	target, err := con.memberInOrg(c, orgID)
	if err != nil {
		return con.failErr(c, err)
	}
	if target.Role == "owner" {
		return con.fail(c, fiber.StatusBadRequest, "no se puede cambiar el rol del owner")
	}

	if err := models.UpdateMembershipRole(c.Context(), con.Env.Postgres, target.ID, b.Role); err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo actualizar el rol")
	}
	return con.ok(c, "rol actualizado", nil)
}

// DELETE /orgs/:orgId/members/:memberId — quita miembro (owner/admin).
func (con *Controller) RemoveMember(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID, "owner", "admin"); err != nil {
		return con.failErr(c, err)
	}
	target, err := con.memberInOrg(c, orgID)
	if err != nil {
		return con.failErr(c, err)
	}
	if target.Role == "owner" {
		return con.fail(c, fiber.StatusBadRequest, "no se puede quitar al owner")
	}
	if err := models.DeleteMembership(c.Context(), con.Env.Postgres, target.ID); err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo quitar el miembro")
	}
	return con.ok(c, "miembro eliminado", nil)
}

// memberInOrg carga el membership :memberId y valida que pertenezca a orgID.
func (con *Controller) memberInOrg(c *fiber.Ctx, orgID string) (*models.Membership, error) {
	target, err := models.GetMembershipByID(c.Context(), con.Env.Postgres, c.Params("memberId"))
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, "error obteniendo el miembro")
	}
	if target == nil || target.OrgID != orgID {
		return nil, fiber.NewError(fiber.StatusNotFound, "miembro no encontrado")
	}
	return target, nil
}
