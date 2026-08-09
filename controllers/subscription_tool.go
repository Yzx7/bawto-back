package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Yzx7/sacs-chatbots/models"
)

// execSubscriptionActivate es deliberadamente distinta de data_mutate: cruza
// organizaciones y, por tanto, verifica que el bot que la invoca sea propiedad
// del dueño único del ledger comercial antes de resolver el código del cliente.
func (con *Controller) execSubscriptionActivate(ctx context.Context, bot *models.BotChannel, phone string, args map[string]string) (string, error) {
	if bot == nil {
		return "", fmt.Errorf("bot no disponible")
	}
	result, err := models.ActivatePlatformSubscription(ctx, con.Env.Postgres, models.ActivateSubscriptionInput{
		SellerOrgID:     bot.OrgID,
		ActivationCode:  strings.TrimSpace(args["activationCode"]),
		PlanKey:         strings.TrimSpace(args["planKey"]),
		BillingCycle:    strings.TrimSpace(args["billingCycle"]),
		Phone:           phone,
		PaymentRecordID: strings.TrimSpace(args["paymentRecordId"]),
		IdempotencyKey:  strings.TrimSpace(args["idempotencyKey"]),
	})
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(result)
	return string(raw), err
}
