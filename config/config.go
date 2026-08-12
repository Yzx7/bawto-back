package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config contiene toda la configuración del servicio, cargada desde
// variables de entorno (y opcionalmente un archivo .env / config.yml).
type Config struct {
	ServerPort string // SERVER_PORT (ej. ":3009")
	AppEnv     string // APP_ENV (dev | prod)

	DatabaseURL string // DATABASE_URL (postgres://user:pass@host:port/db)

	// Clave para cifrar en reposo los tokens de canal (hex/base64 de 32 bytes).
	TokenEncKey string // TOKEN_ENC_KEY

	// Auth: URL del JWKS de Better Auth (frontend) para validar los JWT.
	JWKSURL string // JWKS_URL (ej. http://localhost:3000/api/auth/jwks)

	// WhatsApp Cloud API (nivel app de Meta)
	WhatsAppVerifyToken string // WHATSAPP_VERIFY_TOKEN (verificación del webhook)
	WhatsAppAppSecret   string // WHATSAPP_APP_SECRET  (firma X-Hub-Signature-256 + intercambio Embedded Signup)
	WhatsAppAPIBase     string // WHATSAPP_API_BASE    (default https://graph.facebook.com)
	WhatsAppAPIVersion  string // WHATSAPP_API_VERSION (default v21.0)
	FacebookAppID       string // FACEBOOK_APP_ID      (Embedded Signup: intercambio del code)

	// IA de texto — configuración histórica compatible con Anthropic. Aunque los
	// nombres MINIMAX_* se conservan para no romper los entornos existentes, hoy
	// esta instancia corresponde a DeepSeek y atiende agentes sin visión.
	MinimaxAPIKey             string  // MINIMAX_API_KEY
	MinimaxBaseURL            string  // MINIMAX_BASE_URL (default https://api.deepseek.com/anthropic)
	MinimaxModel              string  // MINIMAX_MODEL   (default deepseek-v4-flash)
	AIProvider                string  // AI_PROVIDER (default deepseek)
	AIInputUSDPerMillion      float64 // AI_INPUT_USD_PER_MILLION
	AIOutputUSDPerMillion     float64 // AI_OUTPUT_USD_PER_MILLION
	AICacheReadUSDPerMillion  float64 // AI_CACHE_READ_USD_PER_MILLION
	AICacheWriteUSDPerMillion float64 // AI_CACHE_WRITE_USD_PER_MILLION
	// AIRatesExplicit distingue "el entorno declaró las tarifas" de "valen cero".
	// Sin esta marca, un override legítimo de $0 (la escritura de caché lo es)
	// no se podría diferenciar de una variable ausente.
	AIRatesExplicit bool

	// IA visual — proveedor deliberadamente separado. Marcar `accepts: image` en
	// un nodo selecciona esta instancia completa; el grafo nunca ve credenciales,
	// endpoint ni nombre de modelo.
	MinimaxM3APIKey  string // MINIMAX_M3_API_KEY
	MinimaxM3BaseURL string // MINIMAX_M3_BASE_URL (default https://api.minimax.io/anthropic)
	MinimaxM3Model   string // MINIMAX_M3_MODEL (default MiniMax-M3)

	// Copilot de autoría de flujos. Es una instancia deliberadamente separada de
	// los agentes que atienden WhatsApp: puede usar otro proveedor/modelo y se
	// deshabilita por sí sola si no está configurada.
	CopilotEnabled           bool          // COPILOT_ENABLED
	CopilotAIProvider        string        // COPILOT_AI_PROVIDER
	CopilotAIBaseURL         string        // COPILOT_AI_BASE_URL
	CopilotAIAPIKey          string        // COPILOT_AI_API_KEY
	CopilotAIModel           string        // COPILOT_AI_MODEL
	CopilotAIReasoningEffort string        // COPILOT_AI_REASONING_EFFORT
	CopilotAIMaxSteps        int           // COPILOT_AI_MAX_STEPS
	CopilotAITimeout         time.Duration // COPILOT_AI_TIMEOUT_SECONDS

	// CORS: orígenes permitidos, separados por coma.
	CorsOrigins string // CORS_ORIGINS

	// ---- Feature flags de la migración multiflujo (PLAN §17) ----
	//
	// Son dos y no uno a propósito: un flag global obligaría a apagar los
	// recordatorios para revertir el dispatcher, que es lo contrario de lo que
	// se querría en un incidente. `FLOWS_TABLE_ENABLED` ya no existe: el webhook
	// lee siempre la versión publicada en `flows`.

	// SchedulerEnabled: cron + worker de entrega. Encendido por defecto porque
	// es lo que hace hoy el arranque; apagarlo deja de encolar y entregar sin
	// borrar runs.
	SchedulerEnabled bool // SCHEDULER_ENABLED

	// MultiFlowDispatchEnabled: sesiones conversacionales y selección entre
	// varios flujos `message` (fase 7). Declarado aquí para que el rollback
	// exista desde el primer día; **hoy no lo lee nadie**.
	MultiFlowDispatchEnabled bool // MULTI_FLOW_DISPATCH_ENABLED

	// Scheduler durable (fase 2).
	SchedulerCatchupWindow    time.Duration // SCHEDULER_CATCHUP_WINDOW
	SchedulerLockTimeout      time.Duration // SCHEDULER_LOCK_TIMEOUT
	SchedulerChatPostpone     time.Duration // SCHEDULER_CHAT_POSTPONE
	SchedulerWABAMPS          float64       // SCHEDULER_WABA_MPS
	ReminderCorrelationWindow time.Duration // REMINDER_CORRELATION_WINDOW
}

// Load lee la configuración desde el entorno. Un archivo .env, si existe, ya
// fue cargado por godotenv en el arranque; aquí solo leemos variables.
func Load() (*Config, error) {
	v := viper.New()
	v.AutomaticEnv()

	// Defaults razonables para desarrollo.
	v.SetDefault("SERVER_PORT", ":3009")
	v.SetDefault("APP_ENV", "dev")
	v.SetDefault("CORS_ORIGINS", "http://localhost:3000")
	v.SetDefault("JWKS_URL", "http://localhost:3000/api/auth/jwks")
	v.SetDefault("WHATSAPP_API_BASE", "https://graph.facebook.com")
	v.SetDefault("WHATSAPP_API_VERSION", "v21.0")
	v.SetDefault("MINIMAX_BASE_URL", "https://api.deepseek.com/anthropic")
	v.SetDefault("MINIMAX_MODEL", "deepseek-v4-flash")
	v.SetDefault("AI_PROVIDER", "deepseek")
	v.SetDefault("MINIMAX_M3_BASE_URL", "https://api.minimax.io/anthropic")
	v.SetDefault("MINIMAX_M3_MODEL", "MiniMax-M3")
	v.SetDefault("COPILOT_ENABLED", false)
	v.SetDefault("COPILOT_AI_REASONING_EFFORT", "high")
	v.SetDefault("COPILOT_AI_MAX_STEPS", 8)
	v.SetDefault("COPILOT_AI_TIMEOUT_SECONDS", 90)
	// Las cuatro tarifas **no** llevan default a propósito. Antes lo tenían, con
	// los precios de MiniMax-M3, y eso convertía un cambio de modelo sin cambio
	// de precios en meses de consumo registrado con la tarifa de otro — sin un
	// solo error, porque el default siempre respondía algo. Ahora el precio sale
	// del catálogo del modelo (ai.ResolvePricing) y estas variables solo existen
	// como override explícito para un cambio de tarifa del proveedor.
	// Sin configurar nada, el scheduler corre.
	v.SetDefault("SCHEDULER_ENABLED", true)
	v.SetDefault("MULTI_FLOW_DISPATCH_ENABLED", false)
	v.SetDefault("SCHEDULER_CATCHUP_WINDOW", "2h")
	v.SetDefault("SCHEDULER_LOCK_TIMEOUT", "10m")
	v.SetDefault("SCHEDULER_CHAT_POSTPONE", "2h")
	v.SetDefault("SCHEDULER_WABA_MPS", 5.0)
	v.SetDefault("REMINDER_CORRELATION_WINDOW", "72h")

	cfg := &Config{
		ServerPort:                v.GetString("SERVER_PORT"),
		AppEnv:                    v.GetString("APP_ENV"),
		DatabaseURL:               v.GetString("DATABASE_URL"),
		TokenEncKey:               v.GetString("TOKEN_ENC_KEY"),
		JWKSURL:                   v.GetString("JWKS_URL"),
		WhatsAppVerifyToken:       v.GetString("WHATSAPP_VERIFY_TOKEN"),
		WhatsAppAppSecret:         v.GetString("WHATSAPP_APP_SECRET"),
		WhatsAppAPIBase:           v.GetString("WHATSAPP_API_BASE"),
		WhatsAppAPIVersion:        v.GetString("WHATSAPP_API_VERSION"),
		FacebookAppID:             v.GetString("FACEBOOK_APP_ID"),
		MinimaxAPIKey:             v.GetString("MINIMAX_API_KEY"),
		MinimaxBaseURL:            v.GetString("MINIMAX_BASE_URL"),
		MinimaxModel:              v.GetString("MINIMAX_MODEL"),
		AIProvider:                v.GetString("AI_PROVIDER"),
		AIInputUSDPerMillion:      v.GetFloat64("AI_INPUT_USD_PER_MILLION"),
		AIOutputUSDPerMillion:     v.GetFloat64("AI_OUTPUT_USD_PER_MILLION"),
		AICacheReadUSDPerMillion:  v.GetFloat64("AI_CACHE_READ_USD_PER_MILLION"),
		AICacheWriteUSDPerMillion: v.GetFloat64("AI_CACHE_WRITE_USD_PER_MILLION"),
		MinimaxM3APIKey:           v.GetString("MINIMAX_M3_API_KEY"),
		MinimaxM3BaseURL:          v.GetString("MINIMAX_M3_BASE_URL"),
		MinimaxM3Model:            v.GetString("MINIMAX_M3_MODEL"),
		CopilotEnabled:            v.GetBool("COPILOT_ENABLED"),
		CopilotAIProvider:         strings.TrimSpace(v.GetString("COPILOT_AI_PROVIDER")),
		CopilotAIBaseURL:          strings.TrimSpace(v.GetString("COPILOT_AI_BASE_URL")),
		CopilotAIAPIKey:           strings.TrimSpace(v.GetString("COPILOT_AI_API_KEY")),
		CopilotAIModel:            strings.TrimSpace(v.GetString("COPILOT_AI_MODEL")),
		CopilotAIReasoningEffort:  strings.TrimSpace(v.GetString("COPILOT_AI_REASONING_EFFORT")),
		CopilotAIMaxSteps:         v.GetInt("COPILOT_AI_MAX_STEPS"),
		CopilotAITimeout:          time.Duration(v.GetInt("COPILOT_AI_TIMEOUT_SECONDS")) * time.Second,
		CorsOrigins:               v.GetString("CORS_ORIGINS"),

		SchedulerEnabled:          v.GetBool("SCHEDULER_ENABLED"),
		MultiFlowDispatchEnabled:  v.GetBool("MULTI_FLOW_DISPATCH_ENABLED"),
		SchedulerCatchupWindow:    v.GetDuration("SCHEDULER_CATCHUP_WINDOW"),
		SchedulerLockTimeout:      v.GetDuration("SCHEDULER_LOCK_TIMEOUT"),
		SchedulerChatPostpone:     v.GetDuration("SCHEDULER_CHAT_POSTPONE"),
		SchedulerWABAMPS:          v.GetFloat64("SCHEDULER_WABA_MPS"),
		ReminderCorrelationWindow: v.GetDuration("REMINDER_CORRELATION_WINDOW"),
	}

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("config: DATABASE_URL es obligatorio")
	}
	if cfg.SchedulerCatchupWindow <= 0 || cfg.SchedulerLockTimeout <= 0 || cfg.SchedulerChatPostpone <= 0 || cfg.SchedulerWABAMPS <= 0 || cfg.ReminderCorrelationWindow <= 0 {
		return nil, fmt.Errorf("config: parámetros del scheduler deben ser mayores que cero")
	}
	if strings.TrimSpace(cfg.AIProvider) == "" {
		return nil, fmt.Errorf("config: AI_PROVIDER no puede estar vacío")
	}
	if !strings.EqualFold(strings.TrimSpace(cfg.AIProvider), "deepseek") {
		return nil, fmt.Errorf(
			"config: AI_PROVIDER debe ser deepseek; los nodos con visión usan MiniMax-M3 mediante MINIMAX_M3_API_KEY")
	}
	// Las cuatro tarifas van juntas o ninguna. Declarar solo algunas dejaría el
	// resto en cero sin avisar, que es peor que no declarar nada: un costo
	// estimado a la baja parece un dato bueno.
	tarifas := []string{
		"AI_INPUT_USD_PER_MILLION", "AI_OUTPUT_USD_PER_MILLION",
		"AI_CACHE_READ_USD_PER_MILLION", "AI_CACHE_WRITE_USD_PER_MILLION",
	}
	declaradas := 0
	for _, nombre := range tarifas {
		if valor, ok := os.LookupEnv(nombre); ok && strings.TrimSpace(valor) != "" {
			declaradas++
		}
	}
	if declaradas > 0 && declaradas < len(tarifas) {
		return nil, fmt.Errorf(
			"config: las tarifas de IA van las cuatro o ninguna (%d declaradas de %d); "+
				"sin ellas el precio sale del catálogo del modelo", declaradas, len(tarifas))
	}
	cfg.AIRatesExplicit = declaradas == len(tarifas)
	if cfg.AIRatesExplicit &&
		(cfg.AIInputUSDPerMillion < 0 || cfg.AIOutputUSDPerMillion < 0 ||
			cfg.AICacheReadUSDPerMillion < 0 || cfg.AICacheWriteUSDPerMillion < 0) {
		return nil, fmt.Errorf("config: las tarifas de IA no pueden ser negativas")
	}

	// Normaliza el puerto: acepta "3009", ":3009" o "host:3009".
	if !strings.Contains(cfg.ServerPort, ":") {
		cfg.ServerPort = ":" + cfg.ServerPort
	}

	return cfg, nil
}

// CopilotReadiness explica si el agente de autoría puede aceptar turnos. Una
// configuración incompleta nunca impide arrancar el backend: solo mantiene el
// Copilot apagado y deja intactas la edición y publicación manuales.
func (cfg *Config) CopilotReadiness() (bool, string) {
	if cfg == nil || !cfg.CopilotEnabled {
		return false, "feature_disabled"
	}
	if cfg.CopilotAIAPIKey == "" {
		return false, "missing_api_key"
	}
	if cfg.CopilotAIProvider == "" {
		return false, "missing_provider"
	}
	if cfg.CopilotAIModel == "" {
		return false, "missing_model"
	}
	if cfg.CopilotAIBaseURL == "" {
		return false, "missing_base_url"
	}
	if cfg.CopilotAIReasoningEffort == "" {
		return false, "missing_reasoning_effort"
	}
	if cfg.CopilotAIMaxSteps < 2 {
		return false, "invalid_max_steps"
	}
	if cfg.CopilotAITimeout <= 0 {
		return false, "invalid_timeout"
	}
	return true, ""
}
