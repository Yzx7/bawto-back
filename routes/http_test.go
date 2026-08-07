package routes

import (
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/env"
)

// Las rutas nuevas de la Fase 5 comparten prefijo con rutas paramétricas
// (`/flows/:flowId`, `/schedule/queue`) y Fiber no avisa de un solapamiento: la
// primera que casa gana y la otra deja de existir en silencio.
//
// Se comprueba contra la tabla de rutas y no con peticiones: el grupo `/bots`
// lleva `VerifyToken` como middleware, así que una petición sin token devuelve
// 401 tanto si la ruta existe como si no. Un test que mirara el código HTTP
// pasaría siempre y no probaría nada.

func registeredPaths(t *testing.T) map[string]bool {
	t.Helper()
	app := fiber.New()
	RegisterHTTP(app, &env.Env{})
	out := map[string]bool{}
	for _, group := range app.GetRoutes() {
		out[group.Method+" "+group.Path] = true
	}
	return out
}

func TestRutasDeLaInterfazOperativaRegistradas(t *testing.T) {
	paths := registeredPaths(t)
	esperadas := []string{
		http.MethodPost + " /bots/:botId/flows/:flowId/duplicate",
		http.MethodPost + " /bots/:botId/flows/:flowId/schedule/preview",
		http.MethodPost + " /bots/:botId/flows/:flowId/schedule/queue",
		http.MethodGet + " /bots/:botId/flows/:flowId/occurrences",
		http.MethodPost + " /chats/:chatId/reset",
		http.MethodPost + " /bots/:botId/schedule/validate-cron",
		http.MethodGet + " /bots/:botId/flow-runs",
		http.MethodGet + " /bots/:botId/flow-runs/:runId",
		http.MethodPost + " /bots/:botId/flow-runs/:runId/retry",
		http.MethodPost + " /bots/:botId/flow-runs/:runId/cancel",
		http.MethodGet + " /bots/:botId/costs",
		http.MethodGet + " /bots/:botId/ai-usage",
		http.MethodGet + " /orgs/:orgId/costs",
		http.MethodGet + " /orgs/:orgId/billing",
		http.MethodGet + " /orgs/:orgId/billing/statements/:statementId",
		// Comparte prefijo con GET /bots/:botId/channel: si alguien registrara
		// `/channel/:algo` antes, esta dejaría de existir sin avisar.
		http.MethodGet + " /bots/:botId/channel/health",
	}
	for _, ruta := range esperadas {
		if !paths[ruta] {
			t.Errorf("ruta no registrada: %s", ruta)
		}
	}
}

// Una key de flujo llamada "validate-cron" es legal (`^[a-z][a-z0-9_-]{0,62}$`),
// así que la validación de cron no puede colgar de `/flows/`: se comería el
// flujo con esa key. Si alguien la mueve ahí, este test lo dice.
func TestValidateCronNoCuelgaDeFlows(t *testing.T) {
	paths := registeredPaths(t)
	if paths[http.MethodPost+" /bots/:botId/flows/validate-cron"] {
		t.Error("`/flows/validate-cron` chocaría con un flujo cuya key sea validate-cron")
	}
}
