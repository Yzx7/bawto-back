package env

import (
	"io"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yzx7/sacs-chatbots/config"
	"github.com/Yzx7/sacs-chatbots/engine/ai"
	"github.com/Yzx7/sacs-chatbots/events"
	"github.com/Yzx7/sacs-chatbots/helpers"
)

// Env es el contenedor de dependencias (inyección explícita).
// Es la única fuente de verdad de infraestructura para controllers y middlewares.
type Env struct {
	Config         *config.Config
	Postgres       *pgxpool.Pool
	Logger         *slog.Logger
	HTTPLogger     *slog.Logger
	WhatsAppLogger *slog.Logger
	LogCloser      io.Closer
	Cipher         *helpers.Cipher // cifrado de secretos de canal (nil si TOKEN_ENC_KEY no está)
	TextAgent      *ai.Agent       // DeepSeek para nodos sin `accepts: image`
	VisionAgent    *ai.Agent       // MiniMax-M3 para nodos con `accepts: image`
	Events         *events.Hub     // eventos de chat en vivo (SSE); nil deshabilita el stream
}
