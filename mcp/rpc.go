package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Yzx7/sacs-chatbots/models"
)

// serverVersion viaja en el handshake. Un cliente que cachea `flow_spec` compara
// además `catalogHash`, que es lo que de verdad caduca sus reglas.
const serverVersion = "0.3.0"

// MCP sobre HTTP es JSON-RPC en el cuerpo de un POST, así que no hace falta
// ninguna dependencia: encoding/json basta. Añadir un SDK traería su propio
// ciclo de versiones para resolver un sobre de veinte líneas.

// protocolVersions son las revisiones de MCP que este servidor entiende, de más
// nueva a más antigua. Se devuelve la que pidió el cliente si está aquí; si no,
// la primera, que es lo que el protocolo espera para que él decida si sigue.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

// toolFault es un fallo que el modelo debe leer y corregir —checksum vencido,
// bot de otra organización, JSON del grafo inválido—. Vuelve como resultado con
// isError, no como error de protocolo: un error JSON-RPC lo absorbe el cliente y
// el modelo se queda sin saber qué reintentar, que es justo lo contrario de lo
// que necesita un ciclo de prueba y error.
type toolFault struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (f *toolFault) Error() string { return f.Message }

func faultf(code, format string, args ...any) *toolFault {
	return &toolFault{Code: code, Message: fmt.Sprintf(format, args...)}
}

// session es la identidad de UNA petición: la fila de la key que la autoriza,
// con su organización y su alcance. Se resuelve desde la cabecera y muere con la
// respuesta.
//
// Es el punto más delicado del servicio. Un mismo proceso atiende a varias
// personas a la vez, así que nada que dependa de quién pregunta puede vivir
// fuera de esta struct. Una key guardada en el Service, en una variable de
// paquete o en una caché compartida filtraría el alcance de una persona a la
// siguiente petición —y además retrasaría la revocación justo lo que durase la
// caché—, que es lo que este diseño existe para impedir.
type session struct {
	store flowStore
	key   *models.MCPKey
	log   *slog.Logger
}

// actor es quién firma las escrituras: la persona que emitió la key. Una key no
// es un usuario, pero `flows.updated_by` tiene que apuntar a alguien real para
// que el panel pueda decir quién dejó el borrador así.
func (s *session) actor() string {
	if s.key == nil || s.key.CreatedBy == nil {
		return ""
	}
	return *s.key.CreatedBy
}

// logf tolera un logger ausente porque el paquete se prueba sin infraestructura
// y porque un fallo al registrar un fallo no debe convertirse en un panic dentro
// del handler.
func (s *session) logf(format string, args ...any) {
	if s.log == nil {
		return
	}
	s.log.Warn(fmt.Sprintf(format, args...), "org", s.key.OrgID, "key", s.key.ID)
}

// handle resuelve un mensaje JSON-RPC completo. Devuelve nil cuando el mensaje
// era una notificación: el protocolo prohíbe contestarlas, incluso para
// reportar un error.
func (s *session) handle(ctx context.Context, raw []byte) *rpcResponse {
	var request rpcRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return &rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: codeParse, Message: "JSON inválido"}}
	}
	if request.JSONRPC != "2.0" || request.Method == "" {
		return &rpcResponse{JSONRPC: "2.0", ID: nullableID(request.ID),
			Error: &rpcError{Code: codeInvalidRequest, Message: "jsonrpc debe ser 2.0 y method obligatorio"}}
	}
	result, err := s.dispatch(ctx, request.Method, request.Params)
	if len(request.ID) == 0 {
		return nil
	}
	if err != nil {
		var rpcErr *rpcError
		if errors.As(err, &rpcErr) {
			return &rpcResponse{JSONRPC: "2.0", ID: request.ID, Error: rpcErr}
		}
		return &rpcResponse{JSONRPC: "2.0", ID: request.ID,
			Error: &rpcError{Code: codeInternal, Message: err.Error()}}
	}
	return &rpcResponse{JSONRPC: "2.0", ID: request.ID, Result: result}
}

func (e *rpcError) Error() string { return e.Message }

func nullableID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func (s *session) dispatch(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return s.initialize(params)
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolDefinitions()}, nil
	case "tools/call":
		return s.callTool(ctx, params)
	// El servidor no publica prompts ni recursos, pero un cliente que los pide
	// sin haber leído `capabilities` no debería morir: una lista vacía es más
	// útil que un -32601 durante el arranque.
	case "resources/list":
		return map[string]any{"resources": []any{}}, nil
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, nil
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "método no soportado: " + method}
	}
}

func (s *session) initialize(params json.RawMessage) (any, error) {
	var input struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &input)
	}
	version := protocolVersions[0]
	for _, supported := range protocolVersions {
		if supported == input.ProtocolVersion {
			version = supported
			break
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "bawto-flows", "version": serverVersion},
		"instructions": alcanceDeLaKey(s) + "Autoría de flujos de Bawto. El ciclo es: flow_spec para aprender las reglas, " +
			"flow_get para traer el flujo entero y su checksum, editar el JSON completo, flow_validate " +
			"cuantas veces haga falta (no escribe ni cuesta), y flow_put con el expectedChecksum del " +
			"flow_get más reciente. flow_put guarda el BORRADOR: no cambia lo que el bot está atendiendo, " +
			"publicar es una acción humana desde el panel.",
	}, nil
}

// alcanceDeLaKey avisa en el propio handshake cuando la credencial está
// acotada. Sin esto, el modelo interpreta un flujo fuera de alcance como un
// flujo borrado y se pone a recrearlo.
func alcanceDeLaKey(s *session) string {
	if !s.key.Scoped() {
		return ""
	}
	return fmt.Sprintf("Esta key solo alcanza %d flujo(s) concretos; el resto de la organización "+
		"responde como si no existiera. Empieza por flow_get sin argumentos para ver qué tienes. ",
		len(s.key.FlowIDs))
}

func (s *session) callTool(ctx context.Context, params json.RawMessage) (any, error) {
	var input struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &input); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "params inválidos: " + err.Error()}
	}
	handler, exists := toolHandlers[input.Name]
	if !exists {
		return nil, &rpcError{Code: codeInvalidParams, Message: "herramienta desconocida: " + input.Name}
	}
	payload, err := handler(s, ctx, input.Arguments)
	if err != nil {
		var fault *toolFault
		if errors.As(err, &fault) {
			return toolResult(fault, true), nil
		}
		// Un fallo de infraestructura tampoco debe tumbar la petición: el modelo
		// lo ve, decide reintentar y el operador lo encuentra en el log.
		s.logf("%s: %v", input.Name, err)
		return toolResult(&toolFault{Code: "internal_error", Message: err.Error()}, true), nil
	}
	return toolResult(payload, false), nil
}

// toolResult serializa indentado: el contenido lo lee un modelo, y un JSON de
// 40 KB en una sola línea se le vuelve ilegible por tramos.
func toolResult(payload any, isError bool) map[string]any {
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		raw = []byte(fmt.Sprintf("{\"code\":\"encode_error\",\"message\":%q}", err.Error()))
		isError = true
	}
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": string(raw)}},
		"isError": isError,
	}
}
