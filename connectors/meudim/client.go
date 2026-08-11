// Package meudim habla con la API de tienda de Meudim (api.meud.im/v1).
//
// No importa nada del proyecto: es un cliente HTTP y sus tipos. Las
// herramientas del flujo lo usan desde `controllers`, que es la capa que sí
// puede leer la conexión de PostgreSQL y descifrar la credencial.
//
// El contrato que impone la forma de este paquete está en QUICKSTART.md de
// Meudim y en su openapi.json:
//
//   - Todo viaja en el envelope {ok, msg, data, metadata}. Un error trae `ok:
//     false` y una causa en `msg` redactada por la tienda; se conserva, porque es
//     la que sirve para decirle al cliente «quedan 2» en vez de «error 400».
//   - Las listas vacías son `data: []`, nunca null.
//   - La tienda se resuelve desde la clave: no hay store_id en ninguna ruta.
//   - 120 peticiones por minuto y clave, con Retry-After en el 429.
package meudim

import (
	"bytes"
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

// DefaultTimeout acota cada petición. Es corto a propósito: el turno del chat
// sostiene un advisory lock mientras se responde, así que una llamada lenta no
// la paga esta función, la paga la conversación entera.
const DefaultTimeout = 6 * time.Second

// maxBodyBytes evita que una respuesta anómala se lea entera en memoria.
const maxBodyBytes = 4 << 20

// Client es una tienda concreta: una URL base y una clave.
type Client struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

// New construye el cliente. `baseURL` ya viene validado contra la lista
// permitida (connectors.ValidateTarget) cuando procede de una conexión guardada.
func New(baseURL, apiKey string, hc *http.Client) (*Client, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errors.New("meudim: la conexión no tiene credencial")
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, errors.New("meudim: la conexión no tiene URL")
	}
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{baseURL: baseURL, apiKey: apiKey, hc: hc}, nil
}

// Error es un fallo clasificado por su causa, no por su texto.
//
// La distinción que importa río arriba es entre «la tienda dijo que no»
// (4xx: no reintentar, contárselo al cliente) y «no pude preguntar»
// (conexión o 5xx: puede reintentarse una vez).
type Error struct {
	StatusCode int
	// Message es el `msg` del envelope. Lo redacta la tienda y suele ser apto
	// para leérselo a una persona: «stock insuficiente para 'X' (disponible: 2)».
	Message string
	// RetryAfter llega en el 429. No se espera dentro del turno; se registra.
	RetryAfter time.Duration
	// Unreachable significa que no hubo respuesta. Repetir un POST así puede
	// duplicar lo que ya se creó, salvo que lleve Idempotency-Key.
	Unreachable bool
}

func (e *Error) Error() string {
	if e.Unreachable {
		return "meudim: sin respuesta: " + e.Message
	}
	return fmt.Sprintf("meudim %d: %s", e.StatusCode, e.Message)
}

// Retryable distingue el fallo transitorio del rechazo.
func (e *Error) Retryable() bool { return e.Unreachable || e.StatusCode >= 500 }

// RateLimited es el 429 del token bucket por clave.
func (e *Error) RateLimited() bool { return e.StatusCode == http.StatusTooManyRequests }

// Unauthorized es la credencial ausente, inválida o revocada.
func (e *Error) Unauthorized() bool { return e.StatusCode == http.StatusUnauthorized }

// Response son los metadatos de una llamada: paginación y presupuesto de rate
// limit restante. Se devuelven aparte del recurso para no ensuciar los tipos del
// dominio con datos de transporte.
type Response struct {
	Total  int
	Limit  int
	Offset int
	// RateLimitRemaining es -1 cuando la respuesta no trajo el header.
	RateLimitRemaining int
}

type envelope struct {
	OK       bool            `json:"ok"`
	Msg      string          `json:"msg"`
	Data     json.RawMessage `json:"data"`
	Metadata struct {
		Total  int `json:"total"`
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	} `json:"metadata"`
}

type request struct {
	method string
	path   string
	query  url.Values
	body   any
	// idempotencyKey convierte un POST en repetible. Sin ella, un POST que no
	// obtuvo respuesta no se reintenta: crearía un segundo pedido.
	idempotencyKey string
}

// do ejecuta la petición y deserializa `data` en out.
//
// Reintenta **una sola vez** y solo cuando repetir es seguro: en un GET siempre,
// y en un POST únicamente si lleva Idempotency-Key, porque entonces Meudim
// devuelve la orden ya creada en vez de crear otra. Es la regla que impide que
// un corte de red se convierta en dos pedidos cobrados al mismo cliente.
func (c *Client) do(ctx context.Context, r request, out any) (Response, error) {
	meta, err := c.attempt(ctx, r, out)
	if err == nil {
		return meta, nil
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || !apiErr.Retryable() {
		return meta, err
	}
	if r.method != http.MethodGet && r.idempotencyKey == "" {
		return meta, err
	}
	return c.attempt(ctx, r, out)
}

func (c *Client) attempt(ctx context.Context, r request, out any) (Response, error) {
	meta := Response{RateLimitRemaining: -1}

	endpoint := c.baseURL + r.path
	if len(r.query) > 0 {
		endpoint += "?" + r.query.Encode()
	}
	var payload io.Reader
	if r.body != nil {
		raw, err := json.Marshal(r.body)
		if err != nil {
			return meta, err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, endpoint, payload)
	if err != nil {
		return meta, err
	}
	// La clave puede ir en tres headers; se usa Authorization, el recomendado.
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", r.idempotencyKey)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return meta, &Error{Unreachable: true, Message: err.Error()}
	}
	defer resp.Body.Close()
	if remaining, convErr := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining")); convErr == nil {
		meta.RateLimitRemaining = remaining
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return meta, &Error{Unreachable: true, Message: err.Error()}
	}
	var env envelope
	// Un cuerpo ilegible no se trata como éxito aunque el status sea 2xx: sin
	// envelope no hay datos, y devolver el cero de `out` sería inventar.
	decodeErr := json.Unmarshal(raw, &env)

	if resp.StatusCode >= 300 || (decodeErr == nil && !env.OK) {
		message := strings.TrimSpace(env.Msg)
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return meta, &Error{
			StatusCode: resp.StatusCode,
			Message:    message,
			RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
		}
	}
	if decodeErr != nil {
		return meta, &Error{StatusCode: resp.StatusCode, Message: "respuesta ilegible: " + decodeErr.Error()}
	}
	meta.Total, meta.Limit, meta.Offset = env.Metadata.Total, env.Metadata.Limit, env.Metadata.Offset
	if out == nil || len(env.Data) == 0 || string(env.Data) == "null" {
		return meta, nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return meta, &Error{StatusCode: resp.StatusCode, Message: "datos ilegibles: " + err.Error()}
	}
	return meta, nil
}

func retryAfter(header string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return 0
}
