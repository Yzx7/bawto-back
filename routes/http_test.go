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
		// La audiencia es endpoint propio a propósito: si acabara siendo un campo
		// del PATCH de metadatos, un `member` podría quitarse la restricción y
		// publicar sin ella. Que la ruta exista es parte del control de acceso.
		http.MethodPut + " /bots/:botId/flows/:flowId/audience",
		// Sin esta acción, sacar a un contacto de la audiencia no surtiría efecto
		// hasta que su conversación expirara y no habría forma de forzarlo.
		http.MethodPost + " /bots/:botId/flows/:flowId/reset-chats",
		// Cuelga de /audience, que ya es una ruta: si alguien registrara
		// `/audience/:algo` antes, el ensayo en seco dejaría de existir sin avisar.
		http.MethodPost + " /bots/:botId/flows/:flowId/audience/preview",
		// Comparte prefijo con /contact-fields: si alguien registrara
		// `/contact-fields/:algo` antes, esta dejaría de existir sin avisar.
		http.MethodGet + " /orgs/:orgId/contact-query-fields",
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

// Las audiencias se retiraron en 019_flows_audience.sql: una audiencia es una
// consulta sobre los datos de la organización, no una tabla propia. Sus cuatro
// rutas no deben volver, ni siquiera "por compatibilidad" — reintroducirlas
// traería de vuelta la tabla que nadie llegó a usar.
func TestRutasDeAudienciasRetiradas(t *testing.T) {
	paths := registeredPaths(t)
	for _, ruta := range []string{
		http.MethodGet + " /bots/:botId/audiences",
		http.MethodPost + " /bots/:botId/audiences",
		http.MethodGet + " /bots/:botId/audiences/:audienceId/contacts",
		http.MethodPost + " /bots/:botId/audiences/:audienceId/contacts/:contactId",
	} {
		if paths[ruta] {
			t.Errorf("la ruta de audiencias volvió: %s", ruta)
		}
	}
}

// Las conexiones externas cuelgan de `/orgs/:orgId`, que ya tiene muchas rutas
// paramétricas. Se comprueba contra la tabla por la misma razón que el resto:
// el grupo lleva VerifyToken, así que una petición sin token responde 401 exista
// la ruta o no, y un test por HTTP pasaría siempre sin probar nada.
func TestRutasDeConexionesExternasRegistradas(t *testing.T) {
	paths := registeredPaths(t)
	esperadas := []string{
		http.MethodGet + " /orgs/:orgId/connections",
		http.MethodPost + " /orgs/:orgId/connections",
		http.MethodPost + " /orgs/:orgId/connections/:key/test",
		http.MethodDelete + " /orgs/:orgId/connections/:key",
	}
	for _, ruta := range esperadas {
		if !paths[ruta] {
			t.Errorf("ruta no registrada: %s", ruta)
		}
	}
}
