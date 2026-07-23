package config

import (
	"fmt"
	"strings"

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
	MinimaxAPIKey  string // MINIMAX_API_KEY
	MinimaxBaseURL string // MINIMAX_BASE_URL (default https://api.minimax.io/anthropic)
	MinimaxModel   string // MINIMAX_MODEL   (default MiniMax-M2)

	// CORS: orígenes permitidos, separados por coma.
	CorsOrigins string // CORS_ORIGINS
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
	v.SetDefault("MINIMAX_MODEL", "MiniMax-M2")

	cfg := &Config{
		ServerPort:          v.GetString("SERVER_PORT"),
		AppEnv:              v.GetString("APP_ENV"),
		DatabaseURL:         v.GetString("DATABASE_URL"),
		TokenEncKey:         v.GetString("TOKEN_ENC_KEY"),
		JWKSURL:             v.GetString("JWKS_URL"),
		WhatsAppVerifyToken: v.GetString("WHATSAPP_VERIFY_TOKEN"),
		WhatsAppAppSecret:   v.GetString("WHATSAPP_APP_SECRET"),
		WhatsAppAPIBase:     v.GetString("WHATSAPP_API_BASE"),
		WhatsAppAPIVersion:  v.GetString("WHATSAPP_API_VERSION"),
		FacebookAppID:       v.GetString("FACEBOOK_APP_ID"),
		MinimaxAPIKey:       v.GetString("MINIMAX_API_KEY"),
		MinimaxBaseURL:      v.GetString("MINIMAX_BASE_URL"),
		MinimaxModel:        v.GetString("MINIMAX_MODEL"),
		CorsOrigins:         v.GetString("CORS_ORIGINS"),
	}

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("config: DATABASE_URL es obligatorio")
	}

	// Normaliza el puerto: acepta "3009", ":3009" o "host:3009".
	if !strings.Contains(cfg.ServerPort, ":") {
		cfg.ServerPort = ":" + cfg.ServerPort
	}

	return cfg, nil
}
