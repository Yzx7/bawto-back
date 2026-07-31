package config

import (
	"testing"
	"time"
)

// Sin configurar nada, el backend debe comportarse como antes de la fase 1:
// lee `bots.flow` y corre el scheduler. Si estos defaults cambian, un deploy
// normal cambiaría de comportamiento sin que nadie lo pidiera.
func TestFeatureFlagsPorDefecto(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/x?sslmode=disable")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.SchedulerEnabled {
		t.Fatal("SCHEDULER_ENABLED debe estar encendido por defecto (es lo que hace hoy el arranque)")
	}
	if cfg.MultiFlowDispatchEnabled {
		t.Fatal("MULTI_FLOW_DISPATCH_ENABLED debe estar apagado por defecto")
	}
	if cfg.SchedulerCatchupWindow != 2*time.Hour || cfg.SchedulerLockTimeout != 10*time.Minute ||
		cfg.SchedulerChatPostpone != 2*time.Hour || cfg.SchedulerWABAMPS != 5 {
		t.Fatalf("defaults scheduler inesperados: %+v", cfg)
	}
	if cfg.ReminderCorrelationWindow != 72*time.Hour {
		t.Fatalf("ventana de correlación inesperada: %s", cfg.ReminderCorrelationWindow)
	}
	if cfg.MinimaxModel != "MiniMax-M3" || cfg.AIProvider != "minimax" ||
		cfg.AIInputUSDPerMillion != 0.30 || cfg.AIOutputUSDPerMillion != 1.20 ||
		cfg.AICacheReadUSDPerMillion != 0.06 || cfg.AICacheWriteUSDPerMillion != 0 {
		t.Fatalf("tarifas IA por defecto inesperadas: %+v", cfg)
	}
}

func TestTarifasIADesdeElEntorno(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/x?sslmode=disable")
	t.Setenv("AI_PROVIDER", "anthropic")
	t.Setenv("AI_INPUT_USD_PER_MILLION", "1")
	t.Setenv("AI_OUTPUT_USD_PER_MILLION", "5")
	t.Setenv("AI_CACHE_READ_USD_PER_MILLION", "0.1")
	t.Setenv("AI_CACHE_WRITE_USD_PER_MILLION", "1.25")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AIProvider != "anthropic" || cfg.AIInputUSDPerMillion != 1 ||
		cfg.AIOutputUSDPerMillion != 5 || cfg.AICacheReadUSDPerMillion != 0.1 ||
		cfg.AICacheWriteUSDPerMillion != 1.25 {
		t.Fatalf("tarifas IA no siguen el entorno: %+v", cfg)
	}
}

func TestFeatureFlagsDesdeElEntorno(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/x?sslmode=disable")
	t.Setenv("SCHEDULER_ENABLED", "false")
	t.Setenv("MULTI_FLOW_DISPATCH_ENABLED", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SchedulerEnabled || !cfg.MultiFlowDispatchEnabled {
		t.Fatalf("los flags no siguen al entorno: %+v", struct{ S, M bool }{
			cfg.SchedulerEnabled, cfg.MultiFlowDispatchEnabled,
		})
	}
}
