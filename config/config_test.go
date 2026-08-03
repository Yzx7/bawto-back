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
	if cfg.MinimaxModel != "deepseek-v4-flash" || cfg.AIProvider != "deepseek" ||
		cfg.MinimaxBaseURL != "https://api.deepseek.com/anthropic" {
		t.Fatalf("modelo o proveedor por defecto inesperados: %+v", cfg)
	}
	if cfg.MinimaxM3Model != "MiniMax-M3" ||
		cfg.MinimaxM3BaseURL != "https://api.minimax.io/anthropic" {
		t.Fatalf("modelo visual por defecto inesperado: %+v", cfg)
	}
	// Las tarifas ya **no** tienen default aquí: las pone el catálogo del modelo
	// (ai.ResolvePricing) al construir el agente. Tenerlas aquí con los precios
	// de M3 convertía un cambio de modelo sin cambio de precios en meses de
	// consumo registrado con la tarifa de otro, sin un solo error.
	if cfg.AIRatesExplicit {
		t.Fatalf("sin variables de tarifa no debería haber override: %+v", cfg)
	}
}

func TestMinimaxM3VisualDesdeElEntorno(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/x?sslmode=disable")
	t.Setenv("MINIMAX_M3_API_KEY", "sk-vision")
	t.Setenv("MINIMAX_M3_BASE_URL", "https://vision.example/anthropic")
	t.Setenv("MINIMAX_M3_MODEL", "MiniMax-M3")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MinimaxM3APIKey != "sk-vision" ||
		cfg.MinimaxM3BaseURL != "https://vision.example/anthropic" ||
		cfg.MinimaxM3Model != "MiniMax-M3" {
		t.Fatalf("configuración visual no sigue el entorno: %+v", cfg)
	}
}

func TestTarifasIADesdeElEntorno(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/x?sslmode=disable")
	t.Setenv("AI_PROVIDER", "deepseek")
	t.Setenv("AI_INPUT_USD_PER_MILLION", "1")
	t.Setenv("AI_OUTPUT_USD_PER_MILLION", "5")
	t.Setenv("AI_CACHE_READ_USD_PER_MILLION", "0.1")
	t.Setenv("AI_CACHE_WRITE_USD_PER_MILLION", "1.25")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AIProvider != "deepseek" || cfg.AIInputUSDPerMillion != 1 ||
		cfg.AIOutputUSDPerMillion != 5 || cfg.AICacheReadUSDPerMillion != 0.1 ||
		cfg.AICacheWriteUSDPerMillion != 1.25 {
		t.Fatalf("tarifas IA no siguen el entorno: %+v", cfg)
	}
}

func TestProveedorDeTextoDistintoADeepSeekEsRechazado(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/x?sslmode=disable")
	t.Setenv("AI_PROVIDER", "minimax")

	if _, err := Load(); err == nil {
		t.Fatal("se esperaba rechazo: la visión se selecciona por capacidad, no cambiando AI_PROVIDER")
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

// Las cuatro tarifas van juntas o ninguna. Declarar solo algunas dejaría el
// resto en cero sin avisar, y un costo estimado a la baja parece un dato bueno.
func TestTarifasDeIAParcialesSonRechazadas(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/x?sslmode=disable")
	t.Setenv("AI_INPUT_USD_PER_MILLION", "0.14")
	t.Setenv("AI_OUTPUT_USD_PER_MILLION", "0.28")

	if _, err := Load(); err == nil {
		t.Fatal("se esperaba rechazo de dos tarifas de cuatro")
	}
}

func TestTarifasDeIACompletasMarcanElOverride(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/x?sslmode=disable")
	t.Setenv("AI_INPUT_USD_PER_MILLION", "0.14")
	t.Setenv("AI_OUTPUT_USD_PER_MILLION", "0.28")
	t.Setenv("AI_CACHE_READ_USD_PER_MILLION", "0.0028")
	t.Setenv("AI_CACHE_WRITE_USD_PER_MILLION", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.AIRatesExplicit {
		t.Fatal("con las cuatro declaradas, AIRatesExplicit debe estar en true")
	}
	// La escritura de caché en cero es un override legítimo: si se confundiera
	// con "no declarada", el override entero se perdería.
	if cfg.AICacheWriteUSDPerMillion != 0 || cfg.AIInputUSDPerMillion != 0.14 {
		t.Errorf("tarifas mal leídas: %+v", cfg)
	}
}

// Sin declarar nada, el precio lo pone el catálogo del modelo, no un default
// que antes vivía aquí con los precios de MiniMax-M3.
func TestSinTarifasNoHayOverride(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/x?sslmode=disable")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AIRatesExplicit {
		t.Fatal("sin variables de tarifa no debería haber override")
	}
}
