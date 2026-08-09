package models

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

const PlatformPaymentMethodsObject = "instrucciones_pago_bawto"

type PaymentInstructionsResult struct {
	Found   bool   `json:"found"`
	Count   int    `json:"count"`
	Message string `json:"message"`
}

type paymentMethod struct {
	Medium      string
	Destination string
	Holder      string
	Currency    string
	Note        string
}

// PaymentInstructionsForOrg lee todos los métodos activos de la organización y
// compone un único mensaje. El modelo no participa: Data decide qué aparece y
// el orden lo fija `prioridad`, con desempate estable en QueryDataRecords.
func PaymentInstructionsForOrg(ctx context.Context, pool *pgxpool.Pool, orgID string) (*PaymentInstructionsResult, error) {
	result, err := QueryDataRecords(ctx, pool, DataQueryInput{
		OrgID: orgID, ObjectKey: PlatformPaymentMethodsObject,
		Fields:  []string{"clave", "medio", "destino", "titular", "moneda", "nota", "prioridad", "activo"},
		Where:   []DataQueryRule{{Field: "activo", Op: "eq", Value: "true"}},
		OrderBy: "prioridad", Limit: 10,
	})
	if err != nil {
		return nil, err
	}
	methods := make([]paymentMethod, 0, len(result.Records))
	for _, record := range result.Records {
		method := paymentMethod{
			Medium:      paymentText(record.Data["medio"]),
			Destination: paymentText(record.Data["destino"]),
			Holder:      paymentText(record.Data["titular"]),
			Currency:    paymentText(record.Data["moneda"]),
			Note:        paymentText(record.Data["nota"]),
		}
		if method.Medium == "" || method.Destination == "" || method.Holder == "" || method.Currency == "" {
			return nil, fmt.Errorf("el método de pago %s está activo pero incompleto", record.RecordID)
		}
		methods = append(methods, method)
	}
	if len(methods) == 0 {
		return &PaymentInstructionsResult{}, nil
	}
	return &PaymentInstructionsResult{
		Found: true, Count: len(methods), Message: renderPaymentInstructions(methods),
	}, nil
}

func renderPaymentInstructions(methods []paymentMethod) string {
	var message strings.Builder
	if len(methods) == 1 {
		message.WriteString("Puedes pagar tu plan Bawto con este medio:\n\n")
	} else {
		message.WriteString("Puedes elegir uno de estos medios para pagar tu plan Bawto:\n\n")
	}
	for index, method := range methods {
		fmt.Fprintf(&message, "%d. %s\nDestino: %s\nTitular: %s\nMoneda: %s", index+1, method.Medium, method.Destination, method.Holder, method.Currency)
		if method.Note != "" {
			message.WriteString("\n")
			message.WriteString(method.Note)
		}
		message.WriteString("\n\n")
	}
	message.WriteString("Cuando termines, envía una captura completa y nítida donde se vean la fecha, el importe y la operación. El acceso se activará sujeto a verificación posterior.")
	return message.String()
}

func paymentText(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}
