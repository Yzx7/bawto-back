package config

import (
	"fmt"
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

	// IA — MiniMax (endpoint compatible con Anthropic)
	MinimaxAPIKey             string  // MINIMAX_API_KEY
	MinimaxBaseURL            string  // MINIMAX_BASE_URL (default https://api.minimax.io/anthropic)
	MinimaxModel              string  // MINIMAX_MODEL   (default MiniMax-M3)
	AIProvider                string  // AI_PROVIDER (default minimax)
	AIInputUSDPerMillion      float64 // AI_INPUT_USD_PER_MILLION
	AIOutputUSDPerMillion     float64 // AI_OUTPUT_USD_PER_MILLION
	AICacheReadUSDPerMillion  float64 // AI_CACHE_READ_USD_PER_MILLION
	AICacheWriteUSDPerMillion float64 // AI_CACHE_WRITE_USD_PER_MILLION

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
	v.SetDefault("MINIMAX_BASE_URL", "https://api.minimax.io/anthropic")
	v.SetDefault("MINIMAX_MODEL", "MiniMax-M3")
	v.SetDefault("AI_PROVIDER", "minimax")
	// MiniMax-M3 pay-as-you-go para contexto <= 512K, consultado el
	// 2026-07-30. MiniMax no publica una tarifa de cache write para M3 y este
	// cliente no crea breakpoints explícitos, por eso su default es cero.
	// Son overrides
	// obligatoriamente configurables: cambiar de modelo sin cambiar precios
	// produciría un reporte engañoso.
	v.SetDefault("AI_INPUT_USD_PER_MILLION", 0.30)
	v.SetDefault("AI_OUTPUT_USD_PER_MILLION", 1.20)
	v.SetDefault("AI_CACHE_READ_USD_PER_MILLION", 0.06)
	v.SetDefault("AI_CACHE_WRITE_USD_PER_MILLION", 0.0)
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
	if strings.TrimSpace(cfg.AIProvider) == "" ||
		cfg.AIInputUSDPerMillion < 0 || cfg.AIOutputUSDPerMillion < 0 ||
		cfg.AICacheReadUSDPerMillion < 0 || cfg.AICacheWriteUSDPerMillion < 0 {
		return nil, fmt.Errorf("config: proveedor y tarifas de IA inválidos")
	}

	// Normaliza el puerto: acepta "3009", ":3009" o "host:3009".
	if !strings.Contains(cfg.ServerPort, ":") {
		cfg.ServerPort = ":" + cfg.ServerPort
	}

	return cfg, nil
}
