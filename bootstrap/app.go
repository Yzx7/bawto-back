package bootstrap

import (
	"context"
	"fmt"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/joho/godotenv"

	"github.com/Yzx7/sacs-chatbots/config"
	"github.com/Yzx7/sacs-chatbots/db"
	"github.com/Yzx7/sacs-chatbots/engine/ai"
	"github.com/Yzx7/sacs-chatbots/env"
	"github.com/Yzx7/sacs-chatbots/helpers"
	"github.com/Yzx7/sacs-chatbots/logger"
	"github.com/Yzx7/sacs-chatbots/middlewares/accesslog"
	"github.com/Yzx7/sacs-chatbots/routes"
	"github.com/Yzx7/sacs-chatbots/scheduler"
	"github.com/Yzx7/sacs-chatbots/types"
)

// Runtime agrupa la app Fiber y sus dependencias, listo para escuchar.
type Runtime struct {
	App *fiber.App
	Env *env.Env
}

// Build arma todo el runtime: config, logger, pool Postgres, app Fiber y rutas.
func Build(ctx context.Context) (*Runtime, error) {
	// .env es opcional (en prod las vars vienen del entorno).
	_ = godotenv.Load()

	logs, err := logger.Init()
	if err != nil {
		return nil, fmt.Errorf("bootstrap: logger: %w", err)
	}

	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("bootstrap: config: %w", err)
	}

	pool, err := db.NewPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: postgres: %w", err)
	}
	if err := db.Ping(ctx, pool); err != nil {
		return nil, fmt.Errorf("bootstrap: ping postgres: %w", err)
	}
	logs.General.Info("PostgreSQL conectado")

	var cph *helpers.Cipher
	if cfg.TokenEncKey != "" {
		if cph, err = helpers.NewCipher(cfg.TokenEncKey); err != nil {
			logs.General.Warn("cifrado deshabilitado (TOKEN_ENC_KEY inválida)", "err", err)
			cph = nil
		}
	}

	var agent *ai.Agent
	if cfg.MinimaxAPIKey != "" {
		agent = ai.New(cfg.MinimaxAPIKey, cfg.MinimaxBaseURL, cfg.MinimaxModel)
		logs.General.Info("IA (MiniMax) habilitada", "model", cfg.MinimaxModel)
	}

	e := &env.Env{
		Config:         cfg,
		Postgres:       pool,
		Logger:         logs.General,
		HTTPLogger:     logs.HTTP,
		WhatsAppLogger: logs.WhatsApp,
		LogCloser:      logs,
		Cipher:         cph,
		Agent:          agent,
	}

	app := fiber.New(fiber.Config{
		AppName: "sacs-chatbots",
	})

	app.Use(cors.New(cors.Config{
		AllowOrigins: cfg.CorsOrigins,
		AllowMethods: "GET,POST,PUT,DELETE,OPTIONS",
		AllowHeaders: "Origin, Content-Type, Accept, Authorization, X-Hub-Signature-256",
	}))
	app.Use(accesslog.Request(e.HTTPLogger))

	// Health check.
	app.Get("/", func(c *fiber.Ctx) error {
		return c.JSON(types.OK("sacs-chatbots up", fiber.Map{"env": cfg.AppEnv}))
	})

	// Rutas de dominio.
	routes.RegisterHTTP(app, e)
	// Solo encola runs; la entrega se habilita cuando exista soporte de plantillas.
	scheduler.Start(ctx, pool, logs.General)
	scheduler.StartDelivery(ctx, pool, cfg, cph, logs.General)

	return &Runtime{App: app, Env: e}, nil
}

// Listen arranca el servidor HTTP.
func (r *Runtime) Listen() error {
	r.Env.Logger.Info("escuchando", "port", r.Env.Config.ServerPort)
	return r.App.Listen(r.Env.Config.ServerPort)
}

// Close libera recursos (pool de conexiones).
func (r *Runtime) Close() {
	if r.Env.Postgres != nil {
		r.Env.Postgres.Close()
	}
	if r.Env.LogCloser != nil {
		_ = r.Env.LogCloser.Close()
	}
}
