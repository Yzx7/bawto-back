// Package datasetapi habla con un dataset genérico servido por HTTP: la mitad
// de la tool `dataset_query` que sí sabe hacer una petición (§3 de
// PLAN-HACKATON-MARCA-BLANCA-Y-MCP.md).
//
// No importa nada del proyecto —igual que connectors/meudim—: las
// herramientas del flujo lo usan desde `controllers`, que es la capa que
// puede leer la conexión de PostgreSQL y descifrar la credencial.
//
// El endpoint real todavía no existe: lo entrega el equipo del hackatón. Por
// eso este cliente es deliberadamente tolerante con la forma de la
// respuesta —acepta `{items:[...], total:n}`, `{data:[...], total:n}` (el
// mismo estilo de envelope que ya usa Meudim) o un array desnudo— en vez de
// imponer un contrato que nadie ha visto todavía. El día que el endpoint
// exista con una forma fija, esta tolerancia puede endurecerse sin tocar el
// resto de la tool.
package datasetapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout acota cada petición, igual que en meudim: el turno del chat
// sostiene un advisory lock mientras se responde, así que una llamada lenta no
// la paga esta función, la paga la conversación entera.
const DefaultTimeout = 6 * time.Second

// maxBodyBytes evita que una respuesta anómala se lea entera en memoria.
const maxBodyBytes = 4 << 20

// Client es un dataset concreto: una URL base y una clave opcional.
type Client struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

// New construye el cliente. `baseURL` ya viene validado contra la lista
// permitida (connectors.ValidateTarget) cuando procede de una conexión
// guardada. La credencial es opcional: a diferencia de Meudim no hay un
// prefijo único que exigir (connectors.ValidateCredential no impone nada
// especial para este driver), y un dataset de solo lectura puede no pedir
// clave.
func New(baseURL, apiKey string, hc *http.Client) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, errors.New("datasetapi: la conexión no tiene URL")
	}
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{baseURL: baseURL, apiKey: strings.TrimSpace(apiKey), hc: hc}, nil
}

// Filter es una condición sobre un campo. Field y Op los fija el autor del
// flujo (o los reescribe el modelo dentro del vocabulario que permite
// `filterFields`); Value puede venir de una variable del flujo.
type Filter struct {
	Field string
	Op    string
	Value string
}

// Query es lo que acaba viajando en la petición. Resource ya llega validado
// por el llamador (sin `..` ni separadores fuera de `/`): aquí solo se
// codifica en la ruta.
type Query struct {
	Resource string
	// Text es la búsqueda libre ("query" en el contrato de la tool).
	Text   string
	Where  []Filter
	Sort   string
	Fields []string
	Limit  int
}

// Record es una fila del dataset, con forma desconocida hasta que el equipo
// publique su contrato: el llamador la proyecta después según `fields`, igual
// que hace `data_query` con un objeto local.
type Record = map[string]any

// Response son los metadatos de la llamada.
type Response struct {
	Total int
}

// Error es un fallo clasificado por su causa, no por su texto — el mismo
// contrato que connectors/meudim.Error. La distinción que importa río arriba
// es entre «el dataset respondió que no» (4xx) y «no pude preguntar» (red o
// 5xx): la primera es la rama `error` del grafo por configuración o dato
// inválido, la segunda es la caída que separa `found=false` de un fallo real.
type Error struct {
	StatusCode  int
	Message     string
	Unreachable bool
}

func (e *Error) Error() string {
	if e.Unreachable {
		return "datasetapi: sin respuesta: " + e.Message
	}
	return fmt.Sprintf("datasetapi %d: %s", e.StatusCode, e.Message)
}

// Retryable distingue el fallo transitorio del rechazo, igual que en meudim.
func (e *Error) Retryable() bool { return e.Unreachable || e.StatusCode >= 500 }

// RateLimited es un 429: el dataset pide frenar, no que se interprete como
// «sin resultados».
func (e *Error) RateLimited() bool { return e.StatusCode == http.StatusTooManyRequests }

// Query ejecuta la lectura contra `{baseURL}/{resource}`.
//
// Nunca falla por «cero resultados»: eso vuelve como una lista vacía, tal
// como exige la regla dura de found=false en el nivel de la tool. Solo falla
// por transporte, un status de error, o una forma de respuesta que no se pudo
// interpretar con ninguna de las tolerancias documentadas en el paquete.
func (c *Client) Query(ctx context.Context, q Query) ([]Record, Response, error) {
	endpoint := c.baseURL + "/" + strings.TrimLeft(q.Resource, "/")

	values := url.Values{}
	if q.Text != "" {
		values.Set("q", q.Text)
	}
	if q.Sort != "" {
		values.Set("sort", q.Sort)
	}
	if q.Limit > 0 {
		values.Set("limit", strconv.Itoa(q.Limit))
	}
	if len(q.Fields) > 0 {
		// Proyección como pista para el dataset, no como garantía: el llamador
		// vuelve a proyectar sobre lo que llegue, así que un servidor que la
		// ignore no rompe nada.
		values.Set("fields", strings.Join(q.Fields, ","))
	}
	// Se codifican como `where.<n>.field/op/value` —el mismo vocabulario que
	// usa la tool en el grafo— y no como `field:op:value` en un solo valor:
	// así un `value` con `:` (una fecha ISO, una URL) no se confunde con el
	// separador de la condición.
	for i, filter := range q.Where {
		idx := strconv.Itoa(i + 1)
		values.Set("where."+idx+".field", filter.Field)
		values.Set("where."+idx+".op", filter.Op)
		values.Set("where."+idx+".value", filter.Value)
	}
	if len(values) > 0 {
		endpoint += "?" + values.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, Response{}, err
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, Response{}, &Error{Unreachable: true, Message: err.Error()}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, Response{}, &Error{Unreachable: true, Message: err.Error()}
	}
	if resp.StatusCode >= 300 {
		return nil, Response{}, &Error{StatusCode: resp.StatusCode, Message: errorMessage(raw, resp.StatusCode)}
	}
	records, total, err := parseRecords(raw)
	if err != nil {
		return nil, Response{}, &Error{StatusCode: resp.StatusCode, Message: "respuesta ilegible: " + err.Error()}
	}
	if total == 0 {
		total = len(records)
	}
	return records, Response{Total: total}, nil
}

// errorMessage intenta leer una causa legible del cuerpo del error, con los
// nombres de campo más comunes en APIs REST. Sin ninguno, cae al texto del
// status HTTP: siempre hay algo que decirle al cliente, nunca un cuerpo crudo.
func errorMessage(raw []byte, status int) string {
	var body struct {
		Message string `json:"message"`
		Msg     string `json:"msg"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil {
		for _, candidate := range []string{body.Message, body.Msg, body.Error} {
			if strings.TrimSpace(candidate) != "" {
				return candidate
			}
		}
	}
	return http.StatusText(status)
}

// parseRecords tolera tres formas de respuesta 2xx: un array desnudo,
// `{items:[...], total:n}` y `{data:[...], total:n}` (con el total también
// aceptado dentro de `metadata.total`, como el envelope de Meudim). Ninguna
// otra forma se adivina: es preferible fallar con claridad a proyectar un
// objeto que no es una lista de registros.
func parseRecords(raw []byte) ([]Record, int, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return []Record{}, 0, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var records []Record
		if err := json.Unmarshal(raw, &records); err != nil {
			return nil, 0, fmt.Errorf("array de registros ilegible: %w", err)
		}
		return nonNilRecords(records), len(records), nil
	}

	var envelope struct {
		Items    []Record `json:"items"`
		Data     []Record `json:"data"`
		Total    int      `json:"total"`
		Metadata struct {
			Total int `json:"total"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, 0, fmt.Errorf("se esperaba un array o un objeto con `items`/`data`: %w", err)
	}
	total := envelope.Total
	if total == 0 {
		total = envelope.Metadata.Total
	}
	switch {
	case envelope.Items != nil:
		return nonNilRecords(envelope.Items), total, nil
	case envelope.Data != nil:
		return nonNilRecords(envelope.Data), total, nil
	default:
		return nil, 0, errors.New("la respuesta no trae `items` ni `data`")
	}
}

func nonNilRecords(records []Record) []Record {
	if records == nil {
		return []Record{}
	}
	return records
}
