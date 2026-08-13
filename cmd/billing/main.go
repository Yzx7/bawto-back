// billing administra precios y estados de cuenta de Bawto sin exponer
// operaciones comerciales a los usuarios de una organización.
//
//	billing configure -org-id UUID -file billing.json
//	billing preview   -org-id UUID -from 2026-07-01 -to 2026-08-01
//	billing issue     -org-id UUID -from 2026-07-01 -to 2026-08-01 -due 2026-08-10
//	billing paid      -statement-id UUID -document-type factura -document-number F001-123
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/Yzx7/sacs-chatbots/models"
)

func main() {
	_ = godotenv.Load()
	if len(os.Args) < 2 {
		fail("acción requerida: configure, preview, issue o paid")
	}
	action := os.Args[1]
	flags := flag.NewFlagSet(action, flag.ExitOnError)
	orgID := flags.String("org-id", "", "organización")
	file := flags.String("file", "", "archivo JSON de configuración")
	fromRaw := flags.String("from", "", "inicio inclusivo YYYY-MM-DD")
	toRaw := flags.String("to", "", "fin exclusivo YYYY-MM-DD")
	dueRaw := flags.String("due", "", "vencimiento YYYY-MM-DD")
	notes := flags.String("notes", "", "nota del estado de cuenta")
	statementID := flags.String("statement-id", "", "estado de cuenta")
	documentType := flags.String("document-type", "", "tipo del documento fiscal externo")
	documentNumber := flags.String("document-number", "", "número del documento fiscal externo")
	creditsAmount := flags.Float64("credits", 0, "monto de créditos a abonar")
	idempotencyKey := flags.String("idempotency-key", "", "identidad estable del ajuste manual")
	_ = flags.Parse(os.Args[2:])

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		fail("DATABASE_URL no está definida")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		fail("PostgreSQL: %v", err)
	}
	defer pool.Close()

	switch action {
	case "credits-overview":
		if *orgID == "" {
			fail("credits-overview requiere -org-id")
		}
		overview, err := models.GetCreditOverview(ctx, pool, *orgID)
		if err != nil {
			fail("credits overview: %v", err)
		}
		printJSON(overview)
	case "credits-add":
		if *orgID == "" || *creditsAmount <= 0 || *idempotencyKey == "" {
			fail("credits-add requiere -org-id, -credits > 0 e -idempotency-key")
		}
		txRecord, err := models.AddCredits(ctx, pool, models.AddCreditsInput{
			OrgID:          *orgID,
			Credits:        *creditsAmount,
			Type:           models.CreditTxRecharge,
			ReferenceType:  models.CreditRefManual,
			Notes:          *notes,
			IdempotencyKey: "manual:" + *orgID + ":" + *idempotencyKey,
			Metadata:       map[string]any{"source": "cli_billing"},
		})
		if err != nil {
			fail("abonar creditos: %v", err)
		}
		printJSON(txRecord)
	case "configure":
		if *orgID == "" || *file == "" {
			fail("configure requiere -org-id y -file")
		}
		raw, err := os.ReadFile(*file)
		if err != nil {
			fail("leer configuración: %v", err)
		}
		var config models.BillingConfig
		if err := json.Unmarshal(raw, &config); err != nil {
			fail("JSON inválido: %v", err)
		}
		if err := models.SyncBillingConfig(ctx, pool, *orgID, config); err != nil {
			fail("configurar: %v", err)
		}
		fmt.Printf("OK: facturación configurada para %s (%d servicios)\n", *orgID, len(config.Rates))
	case "preview":
		from, to := requirePeriod(*fromRaw, *toRaw)
		if *orgID == "" {
			fail("preview requiere -org-id")
		}
		overview, err := models.GetOrganizationBillingOverview(ctx, pool, *orgID, from, to)
		if err != nil {
			fail("preview: %v", err)
		}
		printJSON(overview)
	case "issue":
		from, to := requirePeriod(*fromRaw, *toRaw)
		if *orgID == "" {
			fail("issue requiere -org-id")
		}
		var due time.Time
		if *dueRaw != "" {
			due = parseDate(*dueRaw, "-due")
		}
		statement, err := models.IssueBillingStatement(ctx, pool, *orgID, from, to, due, *notes)
		if err != nil {
			fail("emitir: %v", err)
		}
		printJSON(statement)
	case "paid":
		if *statementID == "" {
			fail("paid requiere -statement-id")
		}
		if err := models.MarkBillingStatementPaid(ctx, pool, *statementID, *documentType, *documentNumber); err != nil {
			fail("marcar pagado: %v", err)
		}
		fmt.Println("OK: estado de cuenta marcado como pagado")
	default:
		fail("acción desconocida %q", action)
	}
}

func requirePeriod(fromRaw, toRaw string) (time.Time, time.Time) {
	if fromRaw == "" || toRaw == "" {
		fail("-from y -to son obligatorios")
	}
	from := parseDate(fromRaw, "-from")
	to := parseDate(toRaw, "-to")
	if !from.Before(to) {
		fail("-from debe ser anterior a -to")
	}
	return from, to
}

func parseDate(raw, label string) time.Time {
	value, err := time.Parse("2006-01-02", raw)
	if err != nil {
		fail("%s inválido: usa YYYY-MM-DD", label)
	}
	return value.UTC()
}

func printJSON(value any) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fail("serializar: %v", err)
	}
	fmt.Println(string(raw))
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERR: "+format+"\n", args...)
	os.Exit(1)
}
