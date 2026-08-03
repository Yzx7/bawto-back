package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Yzx7/sacs-chatbots/models"
)

// execDataMutate traduce la configuración declarativa del bloque a la
// primitiva transaccional. Las únicas claves dinámicas son field.<campo>; no se
// acepta una sentencia, fragmento SQL ni org/object ID proporcionado por datos
// del usuario.
func (con *Controller) execDataMutate(ctx context.Context, bot *models.BotChannel, currentPhone, channelMessageID string, args map[string]string) (string, error) {
	values := make(map[string]any)
	for key, value := range args {
		if !strings.HasPrefix(key, "field.") {
			continue
		}
		field := strings.TrimSpace(strings.TrimPrefix(key, "field."))
		if field == "" {
			return "", fmt.Errorf("field requiere una clave")
		}
		values[field] = value
	}
	linkCurrentContact, err := strconv.ParseBool(defaultString(args["linkCurrentContact"], "false"))
	if err != nil {
		return "", fmt.Errorf("linkCurrentContact debe ser true o false")
	}
	idempotencyKey := strings.TrimSpace(args["idempotencyKey"])
	if idempotencyKey == "" && strings.TrimSpace(channelMessageID) != "" {
		idempotencyKey = "message:" + strings.TrimSpace(channelMessageID)
	}
	result, err := models.MutateDataRecord(ctx, con.Env.Postgres, models.DataMutationInput{
		OrgID: bot.OrgID, ObjectKey: args["object"], Operation: args["operation"],
		RecordID: args["recordId"], MatchField: args["matchField"], MatchValue: args["matchValue"],
		Values: values, CurrentContactPhone: currentPhone, LinkCurrentContact: linkCurrentContact,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(result)
	return string(raw), err
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
