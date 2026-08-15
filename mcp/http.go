package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Service es lo único que se comparte entre peticiones: la infraestructura.
// **No guarda ninguna identidad.**
//
// Esa ausencia es deliberada: un mismo proceso atiende a varias personas, así
// que cualquier dato que dependa de quién pregunta —organización, alcance—
// pertenece a `session`, que nace y muere dentro del handler.
type Service struct {
	store flowStore
	log   *slog.Logger
}

// NewService construye el servicio contra PostgreSQL. Sin pool devuelve un
// servicio **deshabilitado**, no un nil ni uno que acepte cualquier key.
//
// Ya no hay ninguna clave de configuración que valga: la autoridad está en
// `mcp_keys`, así que el servicio queda operativo en cuanto hay base. Es una
// pieza menos que puede faltar en un `.env` y dejar el endpoint muerto sin que
// nadie lo note.
func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	service := &Service{log: log}
	if pool != nil {
		service.store = pgStore{pool: pool}
	}
	return service
}

func (svc *Service) enabled() bool { return svc != nil && svc.store != nil }

// Register monta el transporte Streamable HTTP de MCP.
//
// No cuelga de `authmw.VerifyToken` y no es un descuido: ese middleware valida
// el JWT de sesión que emite el panel, y un cliente MCP no tiene sesión de panel
// —tiene una key de `mcp_keys`, emitida desde el dashboard—. El precedente es el
// grupo `webhook`, que también trae su propia autenticación porque quien llama
// no es el navegador del dueño.
//
// Las rutas se montan **siempre**, aunque el servicio esté deshabilitado, y
// entonces responden 503. Una ruta que desaparece se diagnostica como «el
// endpoint no existe» y manda a buscar el fallo al sitio equivocado.
func (svc *Service) Register(app *fiber.App) {
	group := app.Group("/mcp")
	group.Post("/flows", svc.handlePost)
	// La spec permite que el servidor no ofrezca el canal servidor→cliente, y
	// entonces exige responder 405 en vez de dejar la conexión colgando. Este
	// servicio no envía nada sin que se lo pidan.
	group.Get("/flows", svc.handleUnsupportedStream)
	group.Delete("/flows", svc.handleUnsupportedStream)
}

// handlePost atiende un mensaje JSON-RPC por petición.
func (svc *Service) handlePost(c *fiber.Ctx) error {
	if !svc.enabled() {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "el servicio MCP no tiene base de datos disponible",
		})
	}
	session, failure := svc.authenticate(c)
	if failure != nil {
		return failure(c)
	}
	body := c.Body()
	if len(strings.TrimSpace(string(body))) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(rpcResponse{
			JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: codeParse, Message: "cuerpo vacío"},
		})
	}
	// Un lote de JSON-RPC llega como array. La revisión 2025-06-18 lo retiró del
	// protocolo, así que en vez de implementar a medias un formato que el cliente
	// no debería mandar, se rechaza diciendo qué hacer.
	if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		return c.Status(fiber.StatusBadRequest).JSON(rpcResponse{
			JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: codeInvalidRequest,
				Message: "este servidor no acepta lotes JSON-RPC; manda un mensaje por petición"},
		})
	}

	// El contexto de la petición acota la consulta: si el cliente corta, la
	// transacción no se queda viva del otro lado.
	ctx, cancel := context.WithCancel(c.UserContext())
	defer cancel()

	response := session.handle(ctx, body)
	if response == nil {
		// Era una notificación. La spec pide 202 sin cuerpo, y un JSON aquí haría
		// que algunos clientes lo tomaran por una respuesta huérfana.
		//
		// `Status(...)` y nil, no `SendStatus`: este último rellena el cuerpo con
		// el texto del estado («Accepted») cuando está vacío, y eso ya no es un
		// cuerpo vacío para un cliente que intente parsearlo.
		c.Status(fiber.StatusAccepted)
		return nil
	}
	return c.JSON(response)
}

func (svc *Service) handleUnsupportedStream(c *fiber.Ctx) error {
	return c.Status(fiber.StatusMethodNotAllowed).JSON(fiber.Map{
		"error": "este servidor MCP solo atiende POST; no abre canal de eventos ni mantiene sesión con estado",
	})
}

// authFailure escribe la respuesta de un rechazo de autenticación. Se devuelve
// como función y no como error para que el handler no tenga que decidir el
// código HTTP a partir del texto.
type authFailure func(c *fiber.Ctx) error

// authenticate resuelve la identidad **de esta petición** y solo de esta,
// consultando `mcp_keys` cada vez.
//
// Sin caché a propósito. Una key revocada tiene que dejar de funcionar en la
// llamada siguiente; guardar la fila entre peticiones sería exactamente tanto
// acceso indebido como durase la caché, después de que alguien haya cerrado la
// puerta desde el panel. Es una consulta por índice único sobre un hash, que es
// barata precisamente para poder pagarla siempre.
func (svc *Service) authenticate(c *fiber.Ctx) (*session, authFailure) {
	presented := bearerToken(c.Get(fiber.HeaderAuthorization))
	if presented == "" {
		return nil, unauthorized("falta la cabecera Authorization: Bearer <key>")
	}
	ctx, cancel := context.WithCancel(c.UserContext())
	defer cancel()
	key, err := svc.store.ResolveKey(ctx, presented)
	if err != nil {
		svc.logWarn("no se pudo resolver la key", "motivo", err.Error())
		return nil, func(c *fiber.Ctx) error {
			return c.Status(fiber.StatusServiceUnavailable).
				JSON(fiber.Map{"error": "no se pudo comprobar la credencial"})
		}
	}
	// Desconocida, revocada y caducada comparten respuesta: quien prueba
	// credenciales no debe poder distinguir «nunca existió» de «existió y se la
	// quitaron», que es media pista sobre a quién seguir intentando.
	// La comprobación se repite aquí aunque el SELECT ya filtre por revocación y
	// caducidad, y no es redundancia ociosa: quitar el filtro de la consulta no
	// lo detectaría ninguna prueba de este paquete, porque el stub decide qué
	// encuentra. Esta sí. Es el mismo motivo por el que el Router de Sistemuino
	// vuelve a comprobar `activo` después de que el bloque ya haya filtrado
	// (CLAUDE.md §11).
	if key == nil || !key.Active(time.Now()) {
		return nil, unauthorized("key inválida, revocada o caducada")
	}
	// El uso se anota después de autorizar y sin bloquear la respuesta si falla:
	// no poder escribir una marca de observabilidad no es motivo para negar un
	// acceso ya concedido.
	if err := svc.store.TouchKey(ctx, key.ID); err != nil {
		svc.logWarn("no se pudo anotar el uso de la key", "motivo", err.Error())
	}
	return &session{store: svc.store, key: key, log: svc.log}, nil
}

func unauthorized(message string) authFailure {
	return func(c *fiber.Ctx) error {
		// WWW-Authenticate es lo que hace que un cliente MCP sepa que debe pedir
		// credenciales en vez de dar el servidor por caído.
		c.Set(fiber.HeaderWWWAuthenticate, `Bearer realm="bawto-mcp"`)
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": message})
	}
}

// bearerToken tolera el esquema en cualquier caja porque no todos los clientes
// lo escriben igual, pero no acepta la key desnuda: sin el esquema no hay forma
// de distinguirla de otra credencial.
func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	if len(header) < 7 || !strings.EqualFold(header[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(header[7:])
}

func (svc *Service) logWarn(message string, args ...any) {
	if svc.log == nil {
		return
	}
	svc.log.Warn("mcp: "+message, args...)
}
