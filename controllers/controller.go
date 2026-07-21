package controllers

import (
	"errors"
	"slices"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/env"
	authmw "github.com/Yzx7/sacs-chatbots/middlewares/auth"
	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
)

// Controller agrupa las dependencias (env.Env) y expone los handlers HTTP.
type Controller struct {
	Env *env.Env
}

func New(e *env.Env) *Controller { return &Controller{Env: e} }

// ---- helpers de respuesta (reducen verbosidad) ----

func (con *Controller) ok(c *fiber.Ctx, msg string, data any) error {
	return c.JSON(types.OK(msg, data))
}

func (con *Controller) fail(c *fiber.Ctx, status int, msg string) error {
	return c.Status(status).JSON(types.Err(msg))
}

// failErr traduce un *fiber.Error (típicamente de requireOrgRole) a GenRes.
func (con *Controller) failErr(c *fiber.Ctx, err error) error {
	var fe *fiber.Error
	if errors.As(err, &fe) {
		return con.fail(c, fe.Code, fe.Message)
	}
	return con.fail(c, fiber.StatusInternalServerError, "error inesperado")
}

// requireOrgRole exige que el usuario autenticado tenga uno de `roles` en la org
// (sin roles = basta ser miembro). Reemplaza el bloque de permisos repetido.
// Devuelve su membership, o un *fiber.Error listo para failErr.
func (con *Controller) requireOrgRole(c *fiber.Ctx, orgID string, roles ...string) (*models.Membership, error) {
	claims, _ := authmw.Current(c)
	m, err := models.GetMembership(c.Context(), con.Env.Postgres, claims.UserID, orgID)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, "error verificando permisos")
	}
	if m == nil {
		return nil, fiber.NewError(fiber.StatusForbidden, "no eres miembro de esta organización")
	}
	if len(roles) > 0 && !slices.Contains(roles, m.Role) {
		return nil, fiber.NewError(fiber.StatusForbidden, "permiso insuficiente")
	}
	return m, nil
}
