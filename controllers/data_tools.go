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

// execDataQuery ejecuta la lectura desde un bloque del grafo. Los argumentos
// llegan ya interpolados por el motor, así que `where.1.value` puede contener el
// valor de una variable; `object`, los campos y los operadores no, porque el
// validador los rechaza si traen `{`. Esa asimetría es el contrato: el mensaje
// del cliente decide **qué se busca**, nunca **dónde ni cómo**.
func (con *Controller) execDataQuery(ctx context.Context, bot *models.BotChannel, currentPhone string, args map[string]string) (string, error) {
	linkCurrentContact, err := strconv.ParseBool(defaultString(args["linkCurrentContact"], "false"))
	if err != nil {
		return "", fmt.Errorf("linkCurrentContact debe ser true o false")
	}
	limit := 0
	if raw := strings.TrimSpace(args["limit"]); raw != "" {
		if limit, err = strconv.Atoi(raw); err != nil {
			return "", fmt.Errorf("limit debe ser un número")
		}
	}
	input := models.DataQueryInput{
		OrgID:     bot.OrgID,
		ObjectKey: args["object"],
		Fields:    splitList(args["fields"]),
		Where:     dataQueryRulesFromArgs(args),
		OrderBy:   strings.TrimSpace(args["orderBy"]),
		OrderDesc: strings.EqualFold(strings.TrimSpace(args["orderDir"]), "desc"),
		Limit:     limit,
	}
	if linkCurrentContact {
		input.LinkedContactPhone = currentPhone
	}
	result, err := models.QueryDataRecords(ctx, con.Env.Postgres, input)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(result)
	return string(raw), err
}

// execPaymentMethodsRender mantiene la composición fuera de la IA: el bot fija
// la organización, Data contiene las filas activas y el backend solo las ordena
// y presenta. No recibe argumentos del grafo ni del cliente.
func (con *Controller) execPaymentMethodsRender(ctx context.Context, bot *models.BotChannel) (string, error) {
	result, err := models.PaymentInstructionsForOrg(ctx, con.Env.Postgres, bot.OrgID)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(result)
	return string(raw), err
}

// dataQueryRulesFromArgs recompone las condiciones numeradas del bloque.
//
// Se recorren por índice y no iterando el mapa porque el orden de un `map` en Go
// es aleatorio: sin esto, dos ejecuciones idénticas generarían el mismo SQL con
// los parámetros permutados, y cualquier error mencionaría una condición
// distinta cada vez.
func dataQueryRulesFromArgs(args map[string]string) []models.DataQueryRule {
	var rules []models.DataQueryRule
	for index := 1; index <= maxDataQueryRules; index++ {
		prefix := "where." + strconv.Itoa(index) + "."
		field := strings.TrimSpace(args[prefix+"field"])
		if field == "" {
			continue
		}
		rules = append(rules, models.DataQueryRule{
			Field: field,
			Op:    defaultString(args[prefix+"op"], "eq"),
			Value: args[prefix+"value"],
		})
	}
	return rules
}

// maxDataQueryRules espeja el tope del validador del grafo. Existe aquí para
// acotar el recorrido, no para validar: un bloque publicado ya pasó por engine.
const maxDataQueryRules = 8

func splitList(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
