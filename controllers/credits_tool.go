package controllers

import (
	"context"
	"encoding/json"
	"fmt"
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
	wallet, err := models.RechargePlatformCredits(ctx, con.Env.Postgres, models.RechargePlatformCreditsInput{
		SellerOrgID:     bot.OrgID,
		ActivationCode:  activationCode,
		Phone:           phone,
		PaymentRecordID: strings.TrimSpace(args["paymentRecordId"]),
		Notes:           strings.TrimSpace(args["notes"]),
	})
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(wallet)
	return string(raw), err
}
