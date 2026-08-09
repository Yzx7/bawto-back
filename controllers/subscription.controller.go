package controllers

import (
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
)

func (con *Controller) GetOrganizationSubscription(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID); err != nil {
		return con.failErr(c, err)
	}
	subscription, err := models.GetOrganizationSubscription(c.Context(), con.Env.Postgres, orgID)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo obtener la suscripción")
	}
	return con.ok(c, "ok", subscription)
}

func (con *Controller) ListPlatformSubscriptions(c *fiber.Ctx) error {
	sellerOrgID, err := con.requirePlatformSalesRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	_ = sellerOrgID
	items, err := models.ListPlatformSubscriptions(c.Context(), con.Env.Postgres)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudieron obtener las suscripciones")
	}
	return con.ok(c, "ok", items)
}

func (con *Controller) AssignPlatformSubscription(c *fiber.Ctx) error {
	sellerOrgID, err := con.requirePlatformSalesRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	var body struct {
		ActivationCode string `json:"activationCode"`
		PlanKey        string `json:"planKey"`
		BillingCycle   string `json:"billingCycle"`
		Phone          string `json:"phone"`
	}
	if c.BodyParser(&body) != nil {
		return con.fail(c, fiber.StatusBadRequest, "datos inválidos")
	}
	subscription, err := models.ActivatePlatformSubscription(c.Context(), con.Env.Postgres, models.ActivateSubscriptionInput{
		SellerOrgID: sellerOrgID, ActivationCode: body.ActivationCode, PlanKey: body.PlanKey,
		BillingCycle: body.BillingCycle, Phone: body.Phone,
		IdempotencyKey: fmt.Sprintf("manual:%s:%d", strings.ToUpper(strings.TrimSpace(body.ActivationCode)), time.Now().UnixNano()),
	})
	if err != nil {
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}
	return c.Status(fiber.StatusCreated).JSON(types.OK("suscripción asignada", subscription))
}

func (con *Controller) CancelPlatformSubscription(c *fiber.Ctx) error {
	sellerOrgID, err := con.requirePlatformSalesRole(c)
	if err != nil {
		return con.failErr(c, err)
	}
	var body struct {
		Reason     string `json:"reason"`
		BlockPhone bool   `json:"blockPhone"`
	}
	if c.BodyParser(&body) != nil {
		return con.fail(c, fiber.StatusBadRequest, "datos inválidos")
	}
	subscription, err := models.CancelPlatformSubscription(c.Context(), con.Env.Postgres,
		sellerOrgID, c.Params("recordId"), body.Reason, body.BlockPhone)
	if err != nil {
		return con.fail(c, fiber.StatusBadRequest, "no se pudo anular la suscripción")
	}
	return con.ok(c, "suscripción anulada", subscription)
}

func (con *Controller) requirePlatformSalesRole(c *fiber.Ctx) (string, error) {
	orgID, err := models.PlatformSalesOrgID(c.Context(), con.Env.Postgres)
	if err != nil {
		return "", fiber.NewError(fiber.StatusServiceUnavailable, err.Error())
	}
	if _, err := con.requireOrgRole(c, orgID, "owner", "admin"); err != nil {
		return "", err
	}
	return orgID, nil
}
