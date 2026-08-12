package bootstrap

import (
	"context"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/joho/godotenv"

	"github.com/Yzx7/sacs-chatbots/config"
	"github.com/Yzx7/sacs-chatbots/copilot"
	"github.com/Yzx7/sacs-chatbots/db"
	"github.com/Yzx7/sacs-chatbots/engine/ai"
	"github.com/Yzx7/sacs-chatbots/env"
	"github.com/Yzx7/sacs-chatbots/events"
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
	// stop corta los workers de fondo (scheduler, LISTEN de eventos) antes de
	// cerrar el pool: si no, esas conexiones quedan tomadas y Close se cuelga.
	stop context.CancelFunc
}

// Build arma todo el runtime: config, logger, pool Postgres, app Fiber y rutas.
func Build(ctx context.Context) (*Runtime, error) {
	// .env es opcional (en prod las vars vienen del entorno).
	_ = godotenv.Load()

	ctx, stop := context.WithCancel(ctx)
	// fail aborta el arranque liberando los workers ya lanzados.
	fail := func(format string, err error) (*Runtime, error) {
		stop()
		return nil, fmt.Errorf(format, err)
	}

	logs, err := logger.Init()
	if err != nil {
		return fail("bootstrap: logger: %w", err)
	}

	cfg, err := config.Load()
	if err != nil {
		return fail("bootstrap: config: %w", err)
	}

	pool, err := db.NewPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return fail("bootstrap: postgres: %w", err)
	}
	if err := db.Ping(ctx, pool); err != nil {
		return fail("bootstrap: ping postgres: %w", err)
	}
	logs.General.Info("PostgreSQL conectado")

	var cph *helpers.Cipher
	if cfg.TokenEncKey != "" {
		if cph, err = helpers.NewCipher(cfg.TokenEncKey); err != nil {
			logs.General.Warn("cifrado deshabilitado (TOKEN_ENC_KEY inválida)", "err", err)
			cph = nil
		}
	}

	// El precio sale del catálogo del modelo salvo que el entorno declare las
	// cuatro tarifas. Si no se puede resolver, el arranque falla: el modo de
	// fallo alternativo es un backend sano registrando consumo con la tarifa de
	// otro modelo, y esas filas no se pueden corregir después.
	var textAgent *ai.Agent
	if cfg.MinimaxAPIKey != "" {
		var override *ai.Rates
		if cfg.AIRatesExplicit {
			override = &ai.Rates{
				InputPerMillion:      cfg.AIInputUSDPerMillion,
				OutputPerMillion:     cfg.AIOutputUSDPerMillion,
				CacheReadPerMillion:  cfg.AICacheReadUSDPerMillion,
				CacheWritePerMillion: cfg.AICacheWriteUSDPerMillion,
			}
		}
		pricing, err := ai.ResolvePricing(cfg.AIProvider, cfg.MinimaxModel, override)
		if err != nil {
			return fail("bootstrap: tarifario de IA: %w", err)
		}
		textAgent = ai.NewWithPricing(cfg.MinimaxAPIKey, cfg.MinimaxBaseURL,
			pricing.Provider, cfg.MinimaxModel, pricing.Rates)
		logs.General.Info("IA de texto habilitada",
			"provider", pricing.Provider, "model", cfg.MinimaxModel,
			"input_usd_per_million", pricing.Rates.InputPerMillion,
			"output_usd_per_million", pricing.Rates.OutputPerMillion,
			"cache_read_usd_per_million", pricing.Rates.CacheReadPerMillion,
			"tarifario", pricing.Source)
	}

	var visionAgent *ai.Agent
	if cfg.MinimaxM3APIKey != "" {
		pricing, err := ai.ResolvePricing("minimax", cfg.MinimaxM3Model, nil)
		if err != nil {
			return fail("bootstrap: tarifario de IA visual: %w", err)
		}
		visionAgent = ai.NewWithPricing(cfg.MinimaxM3APIKey, cfg.MinimaxM3BaseURL,
			pricing.Provider, cfg.MinimaxM3Model, pricing.Rates)
		logs.General.Info("IA visual habilitada",
			"provider", pricing.Provider, "model", cfg.MinimaxM3Model,
			"input_usd_per_million", pricing.Rates.InputPerMillion,
			"output_usd_per_million", pricing.Rates.OutputPerMillion,
			"cache_read_usd_per_million", pricing.Rates.CacheReadPerMillion,
			"tarifario", pricing.Source)
	}

	// Copilot de autoría: se construye solo si la configuración está completa.
	// Su ausencia degrada en silencio únicamente al Copilot; el editor manual y
	// la publicación siguen funcionando igual.
	var copilotRunner *copilot.Runner
	if ready, reason := cfg.CopilotReadiness(); ready {
		copilotRates := copilot.Rates{}
		pricing, pricingErr := ai.ResolvePricing(cfg.CopilotAIProvider, cfg.CopilotAIModel, nil)
		if pricingErr != nil {
			// Un modelo fuera del catálogo no aborta el arranque: el Copilot queda
			// habilitado pero registrará consumo sin coste hasta que se añada su
			// tarifario. El log lo deja visible.
			logs.General.Warn("copilot: tarifario desconocido; el consumo se registrará sin coste",
				"provider", cfg.CopilotAIProvider, "model", cfg.CopilotAIModel, "err", pricingErr)
		} else {
			copilotRates = copilot.Rates{
				InputPerMillion:      pricing.Rates.InputPerMillion,
				OutputPerMillion:     pricing.Rates.OutputPerMillion,
				CacheReadPerMillion:  pricing.Rates.CacheReadPerMillion,
				CacheWritePerMillion: pricing.Rates.CacheWritePerMillion,
			}
		}
		copilotProvider := copilot.NewAnthropicProvider(
			cfg.CopilotAIAPIKey, cfg.CopilotAIBaseURL, cfg.CopilotAIProvider,
			cfg.CopilotAIModel, cfg.CopilotAIReasoningEffort, copilotRates)
		copilotRunner = copilot.NewRunner(&copilot.Agent{
			Provider: copilotProvider,
			Config: copilot.RunnerConfig{
				MaxSteps: cfg.CopilotAIMaxSteps,
				Timeout:  cfg.CopilotAITimeout,
			},
		})
		logs.General.Info("Copilot de autoría habilitado",
			"provider", cfg.CopilotAIProvider, "model", cfg.CopilotAIModel,
			"reasoning", cfg.CopilotAIReasoningEffort, "max_steps", cfg.CopilotAIMaxSteps)
	} else {
		logs.General.Info("Copilot de autoría deshabilitado", "razón", reason)
	}

	// Eventos de chat en vivo: viajan por LISTEN/NOTIFY, así que funcionan igual
	// con varias instancias del backend.
	hub := events.NewHub(pool, logs.General)
	hub.Start(ctx)

	e := &env.Env{
		Config:            cfg,
		Postgres:          pool,
		Logger:            logs.General,
		HTTPLogger:        logs.HTTP,
		WhatsAppLogger:    logs.WhatsApp,
		LogCloser:         logs,
		Cipher:            cph,
		TextAgent:         textAgent,
		OrchestratorAgent: visionAgent,
		VisionAgent:       visionAgent,
		Copilot:           copilotRunner,
		Events:            hub,
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
	// SCHEDULER_ENABLED (§17) es el kill switch: apagarlo detiene el cron y la
	// entrega sin borrar runs, y el webhook entrante sigue atendiendo.
	if cfg.SchedulerEnabled {
		scheduler.Start(ctx, pool, cfg, logs.General)
		scheduler.StartDelivery(ctx, pool, cfg, cph, hub, logs.General)
	} else {
		logs.General.Warn("scheduler y entrega deshabilitados por SCHEDULER_ENABLED=false")
	}
	return &Runtime{App: app, Env: e, stop: stop}, nil
}

// Listen arranca el servidor HTTP.
func (r *Runtime) Listen() error {
	r.Env.Logger.Info("escuchando", "port", r.Env.Config.ServerPort)
	return r.App.Listen(r.Env.Config.ServerPort)
}

// Close libera recursos (pool de conexiones).
func (r *Runtime) Close() {
	if r.stop != nil {
		r.stop()
		time.Sleep(200 * time.Millisecond) // que los workers suelten sus conexiones
	}
	if r.Env.Postgres != nil {
		r.Env.Postgres.Close()
	}
	if r.Env.LogCloser != nil {
		_ = r.Env.LogCloser.Close()
	}
}
