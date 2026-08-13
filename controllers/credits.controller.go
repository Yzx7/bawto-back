package controllers

import (
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"

	authmw "github.com/Yzx7/sacs-chatbots/middlewares/auth"
	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
)

// GetOrganizationCredits devuelve el resumen del monedero, llamadas restantes e historial reciente.
func (con *Controller) GetOrganizationCredits(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID); err != nil {
		return con.failErr(c, err)
	}
	overview, err := models.GetCreditOverview(c.Context(), con.Env.Postgres, orgID)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudo obtener el estado de créditos")
	}
	return con.ok(c, "ok", overview)
}

// ListOrganizationCreditTransactions devuelve los movimientos paginados de la organización.
func (con *Controller) ListOrganizationCreditTransactions(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID); err != nil {
		return con.failErr(c, err)
	}
	limit, _ := strconv.Atoi(c.Query("limit", "20"))
	var beforeTime *time.Time
	if beforeStr := c.Query("before"); beforeStr != "" {
		if t, err := time.Parse(time.RFC3339, beforeStr); err == nil {
			beforeTime = &t
		}
	}
	txs, err := models.ListCreditTransactions(c.Context(), con.Env.Postgres, orgID, limit, beforeTime)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudieron listar los movimientos de créditos")
	}
	return con.ok(c, "ok", txs)
}

// UpdateOrganizationCreditSettings actualiza umbrales de alerta y sobregiro.
func (con *Controller) UpdateOrganizationCreditSettings(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID, "owner", "admin"); err != nil {
		return con.failErr(c, err)
	}
	var body struct {
		LowBalanceThreshold float64 `json:"lowBalanceThreshold"`
		AllowOverage        bool    `json:"allowOverage"`
		OverageLimit        float64 `json:"overageLimit"`
	}
	if err := c.BodyParser(&body); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "datos inválidos")
	}
	wallet, err := models.UpdateWalletSettings(c.Context(), con.Env.Postgres, orgID,
		body.LowBalanceThreshold, body.AllowOverage, body.OverageLimit)
	if err != nil {
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}
	return con.ok(c, "ajustes de créditos actualizados", wallet)
}

// RechargeOrganizationCredits abona créditos de forma manual o administrativa directa.
func (con *Controller) RechargeOrganizationCredits(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	mem, err := con.requireOrgRole(c, orgID, "owner", "admin")
	if err != nil {
		return con.failErr(c, err)
	}
	var body struct {
		Credits float64 `json:"credits"`
		Notes   string  `json:"notes"`
	}
	if err := c.BodyParser(&body); err != nil || body.Credits <= 0 {
		return con.fail(c, fiber.StatusBadRequest, "el monto de créditos debe ser mayor a cero")
	}
	txRecord, err := models.AddCredits(c.Context(), con.Env.Postgres, models.AddCreditsInput{
		OrgID:         orgID,
		Credits:       body.Credits,
		Type:          models.CreditTxRecharge,
		ReferenceType: models.CreditRefManual,
		ReferenceID:   mem.UserID,
		Notes:         body.Notes,
		Metadata: map[string]any{
			"user_id": mem.UserID,
			"source":  "api_recharge",
		},
	})
	if err != nil {
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}
	return c.Status(fiber.StatusCreated).JSON(types.OK("créditos abonados", txRecord))
}

// RequestOrganizationCreditRecharge registra un cobro en estado pendiente en la tabla cobros de Sistemuino.
func (con *Controller) RequestOrganizationCreditRecharge(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID, "owner", "admin"); err != nil {
		return con.failErr(c, err)
	}
	var body struct {
		AmountPen  float64 `json:"amountPen"`
		Credits    float64 `json:"credits"`
		Operation  string  `json:"operation"`
		PayerPhone string  `json:"payerPhone"`
		Notes      string  `json:"notes"`
	}
	if err := c.BodyParser(&body); err != nil {
		return con.fail(c, fiber.StatusBadRequest, "datos inválidos")
	}
	if body.AmountPen <= 0 && body.Credits <= 0 {
		return con.fail(c, fiber.StatusBadRequest, "el monto en soles o créditos debe ser mayor a cero")
	}

	userEmail := ""
	if claims, ok := authmw.Current(c); ok {
		userEmail = claims.Email
	}

	req, err := models.RequestPlatformCreditRecharge(c.Context(), con.Env.Postgres, models.RequestCreditRechargeInput{
		OrgID:      orgID,
		AmountPen:  body.AmountPen,
		Credits:    body.Credits,
		Operation:  body.Operation,
		PayerPhone: body.PayerPhone,
		Notes:      body.Notes,
		UserEmail:  userEmail,
	})
	if err != nil {
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}
	return c.Status(fiber.StatusCreated).JSON(types.OK("Solicitud de recarga registrada en estado pendiente de confirmación", req))
}

// ListOrganizationRechargeRequests devuelve las solicitudes de recarga de la organización.
func (con *Controller) ListOrganizationRechargeRequests(c *fiber.Ctx) error {
	orgID := c.Params("orgId")
	if _, err := con.requireOrgRole(c, orgID); err != nil {
		return con.failErr(c, err)
	}
	items, err := models.ListPlatformCreditRecharges(c.Context(), con.Env.Postgres, "", orgID)
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudieron obtener las solicitudes de recarga")
	}
	return con.ok(c, "ok", items)
}

// ListPlatformCreditRecharges lista las solicitudes de cobro/recarga desde el panel comercial de Sistemuino.
func (con *Controller) ListPlatformCreditRecharges(c *fiber.Ctx) error {
	if _, err := con.requirePlatformSalesRole(c); err != nil {
		return con.failErr(c, err)
	}
	status := c.Query("status")
	items, err := models.ListPlatformCreditRecharges(c.Context(), con.Env.Postgres, status, "")
	if err != nil {
		return con.fail(c, fiber.StatusInternalServerError, "no se pudieron obtener los cobros")
	}
	return con.ok(c, "ok", items)
}

// ApprovePlatformCreditRecharge valida y confirma el cobro y abona los créditos a la organización.
func (con *Controller) ApprovePlatformCreditRecharge(c *fiber.Ctx) error {
	if _, err := con.requirePlatformSalesRole(c); err != nil {
		return con.failErr(c, err)
	}
	recordID := c.Params("recordId")
	approverEmail := ""
	if claims, ok := authmw.Current(c); ok {
		approverEmail = claims.Email
	}

	wallet, tx, err := models.ApprovePlatformCreditRecharge(c.Context(), con.Env.Postgres, recordID, approverEmail)
	if err != nil {
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}
	return con.ok(c, "Recarga validada y créditos abonados con éxito", fiber.Map{
		"wallet":      wallet,
		"transaction": tx,
	})
}

// RejectPlatformCreditRecharge rechaza una solicitud de recarga por comprobante inválido.
func (con *Controller) RejectPlatformCreditRecharge(c *fiber.Ctx) error {
	if _, err := con.requirePlatformSalesRole(c); err != nil {
		return con.failErr(c, err)
	}
	recordID := c.Params("recordId")
	var body struct {
		Reason string `json:"reason"`
	}
	_ = c.BodyParser(&body)
	rejecterEmail := ""
	if claims, ok := authmw.Current(c); ok {
		rejecterEmail = claims.Email
	}

	if err := models.RejectPlatformCreditRecharge(c.Context(), con.Env.Postgres, recordID, body.Reason, rejecterEmail); err != nil {
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}
	return con.ok(c, "Solicitud de recarga rechazada", nil)
}
