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
	activationCode := strings.TrimSpace(args["activationCode"])
	if activationCode == "" {
		return "", fmt.Errorf("código de activación es obligatorio")
	}
	credits, err := strconv.ParseFloat(strings.TrimSpace(args["creditsAmount"]), 64)
	if err != nil || credits <= 0 {
		// Si no viene creditsAmount explícito, calcularlo por la tasa base (S/ 5 = 100 créditos -> 1 sol = 20 créditos)
		amount, aErr := strconv.ParseFloat(strings.TrimSpace(args["amount"]), 64)
		if aErr == nil && amount > 0 {
			credits = amount * 20.0
		} else {
			return "", fmt.Errorf("monto de créditos inválido: %s", args["creditsAmount"])
		}
	}
	wallet, err := models.RechargePlatformCredits(ctx, con.Env.Postgres, models.RechargePlatformCreditsInput{
		SellerOrgID:     bot.OrgID,
		ActivationCode:  activationCode,
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
