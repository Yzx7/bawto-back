package models

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Esta prueba ejecuta la consulta real aunque el tenant no exista: PostgreSQL
// valida sus alias y joins antes de devolver cero filas, que es justo la
// regresión que dejó invisible el reporte completo del dashboard.
func TestReporteDeCostosCompilaLaConsultaDeWhatsApp(t *testing.T) {
	ctx, pool := costTestPool(t)

	report := &CostReport{WhatsApp: WhatsAppCostSummary{ByCategory: make([]WhatsAppCategoryCost, 0)}}
	from := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	if err := loadWhatsAppCost(ctx, pool, report,
		"00000000-0000-0000-0000-000000000000", "", from, to); err != nil {
		t.Fatalf("consulta de costos de WhatsApp: %v", err)
	}
}

func TestAnaliticaDeIACompilaTodasSusConsultas(t *testing.T) {
	ctx, pool := costTestPool(t)
	from := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)

	usage, err := GetAIUsageAnalytics(ctx, pool,
		"00000000-0000-0000-0000-000000000000", "", "", from, to)
	if err != nil {
		t.Fatalf("consultas de analítica IA: %v", err)
	}
	if len(usage.ByHour) != 24 {
		t.Fatalf("perfil horario incompleto: %d horas", len(usage.ByHour))
	}
}

func costTestPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL no seteada; se omite el test de integración")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("conectar PostgreSQL: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool
}
