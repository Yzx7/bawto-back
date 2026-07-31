// costreport imprime el mismo reporte que GET /bots/:botId/costs sin requerir
// sesión web. Es una herramienta operativa de solo lectura para verificar
// despliegues y conciliar consumo.
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
	botID := flag.String("bot-id", "", "bot cuyo consumo se calculará")
	fromRaw := flag.String("from", "", "inicio YYYY-MM-DD (default: inicio del mes UTC)")
	toRaw := flag.String("to", "", "fin exclusivo YYYY-MM-DD (default: ahora)")
	flag.Parse()
	if *botID == "" {
		fail("-bot-id es obligatorio")
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		fail("DATABASE_URL no está definida")
	}
	now := time.Now().UTC()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	to := now
	var err error
	if *fromRaw != "" {
		from, err = time.Parse("2006-01-02", *fromRaw)
		if err != nil {
			fail("-from inválido")
		}
	}
	if *toRaw != "" {
		to, err = time.Parse("2006-01-02", *toRaw)
		if err != nil {
			fail("-to inválido")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		fail("PostgreSQL: %v", err)
	}
	defer pool.Close()

	bot, err := models.GetBot(ctx, pool, *botID)
	if err != nil || bot == nil {
		fail("bot no encontrado: %v", err)
	}
	report, err := models.GetCostReport(ctx, pool, bot.OrgID, bot.ID, from, to)
	if err != nil {
		fail("calcular consumo: %v", err)
	}
	out, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(out))
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERR: "+format+"\n", args...)
	os.Exit(1)
}
