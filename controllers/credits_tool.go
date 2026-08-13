package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Yzx7/sacs-chatbots/models"
)

// execCreditRechargeActivate valida que el bot pertenezca a la organización comercial y acredita los créditos a la organización indicada.
func (con *Controller) execCreditRechargeActivate(ctx context.Context, bot *models.BotChannel, phone string, args map[string]string) (string, error) {
	if bot == nil {
		return "", fmt.Errorf("bot no disponible")
	}
	credits, err := strconv.ParseFloat(strings.TrimSpace(args["creditsAmount"]), 64)
	if err != nil || credits <= 0 {
		return "", fmt.Errorf("monto de créditos inválido: %s", args["creditsAmount"])
	}
	wallet, err := models.RechargePlatformCredits(ctx, con.Env.Postgres, models.RechargePlatformCreditsInput{
		SellerOrgID:     bot.OrgID,
		ActivationCode:  strings.TrimSpace(args["activationCode"]),
		Credits:         credits,
		Phone:           phone,
		PaymentRecordID: strings.TrimSpace(args["paymentRecordId"]),
		Notes:           strings.TrimSpace(args["notes"]),
		IdempotencyKey:  strings.TrimSpace(args["idempotencyKey"]),
	})
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(wallet)
	return string(raw), err
}
