// Package meudprov aprovisiona tiendas en MEUD: crea la tienda de una
// organización y, cuando el cliente lo pide, le da acceso a verla en meud.im.
//
// Es una superficie **distinta** de connectors/meudim, y por eso es un paquete
// distinto y no un método más de aquel cliente:
//
//   - `api.meud.im/v1` es pública, viaja por internet y está acotada a la tienda
//     de la `sk_` que la firma. La opera el bot.
//   - Esto es interno: escucha en `127.0.0.1:8865` del VPS —ni internet ni la
//     VPN lo alcanzan—, trabaja **por encima** de las tiendas y su credencial
//     puede crear tiendas sin sesión humana. Solo la usa el backend.
//
// Confundirlas sería confundir sus credenciales, que es el error caro: la clave
// de aprovisionamiento vale para todas las tiendas de todos los clientes.
//
// El contrato está en GUIA-CONEXION-MEUD.md, escrito por el equipo de MEUD.
package meudprov

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL es donde escucha el aprovisionamiento en el VPS. No está
// publicado en nginx a propósito: su alcance no debe depender de que una regla
// de firewall siga siendo correcta dentro de seis meses.
const DefaultBaseURL = "http://127.0.0.1:8865/internal/provision"

// DefaultTimeout acota cada petición. Es más largo que el del cliente de
// catálogo porque crear una tienda escribe varias tablas y nadie está esperando
// dentro de un turno de chat: quien espera es una persona mirando el asistente.
const DefaultTimeout = 15 * time.Second

const maxBodyBytes = 1 << 20

// Client es el aprovisionamiento de un servidor MEUD concreto.
type Client struct {
	baseURL string
	key     string
	hc      *http.Client
}

// New construye el cliente y rechaza los destinos con los que la credencial
// viajaría en claro por la red.
//
// La comprobación es de destino, no de configuración: `http` solo se acepta
// contra loopback, que es el caso real —el listener vive en la misma máquina—.
// Contra cualquier otro host se exige `https`, porque esta clave puede crear
// tiendas a nombre de todos los clientes y no debe cruzar ni siquiera la VPN sin
// cifrar.
func New(baseURL, key string, hc *http.Client) (*Client, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("meudprov: falta la clave de aprovisionamiento")
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, errors.New("meudprov: falta la URL de aprovisionamiento")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("meudprov: URL inválida: %w", err)
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if ip := net.ParseIP(parsed.Hostname()); ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf(
				"meudprov: http solo se admite contra loopback; %q exigiría https", parsed.Host)
		}
	default:
		return nil, fmt.Errorf("meudprov: esquema %q no admitido", parsed.Scheme)
	}
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{baseURL: baseURL, key: key, hc: hc}, nil
}

// Error es un fallo del aprovisionamiento clasificado por su causa. Cada código
// pide una reacción distinta y todas están en la tabla del §5 de la guía, así
// que se conservan en vez de aplanarlos a «no se pudo».
type Error struct {
	StatusCode int
	// Message es el `msg` del envelope. MEUD lo redacta pensando en que se le
	// muestre a una persona —el 404 de admins es literalmente eso—, así que se
	// conserva tal cual.
	Message string
	// Unreachable significa que no hubo respuesta. Con Idempotency-Key repetir es
	// seguro; sin ella, no.
	Unreachable bool
}

func (e *Error) Error() string {
	if e.Unreachable {
		return "meudprov: sin respuesta: " + e.Message
	}
	return fmt.Sprintf("meudprov %d: %s", e.StatusCode, e.Message)
}

// Retryable distingue el fallo transitorio del rechazo.
func (e *Error) Retryable() bool { return e.Unreachable || e.StatusCode >= 500 }

// Unauthorized es la clave ausente o desincronizada con el `.env` de MEUD.
// No se reintenta en bucle: no va a arreglarse sola.
func (e *Error) Unauthorized() bool { return e.StatusCode == http.StatusUnauthorized }

// NotFound son dos cosas según la ruta: el correo sin cuenta en meud.im, o la
// tienda que no existe.
func (e *Error) NotFound() bool { return e.StatusCode == http.StatusNotFound }

// Conflict es «ya es miembro de esa tienda»: el estado deseado ya se cumple.
func (e *Error) Conflict() bool { return e.StatusCode == http.StatusConflict }

// Unavailable es el aprovisionamiento sin configurar en el servidor de MEUD.
func (e *Error) Unavailable() bool { return e.StatusCode == http.StatusServiceUnavailable }

// Store es una tienda recién aprovisionada.
type Store struct {
	// ID es un entero, no un UUID.
	ID int64 `json:"storeId"`
	// Created distingue la creación del reintento idempotente.
	Created bool `json:"created"`
	// SK llega **una sola vez**, en el 201. MEUD no la guarda en claro y hoy no
	// hay endpoint de reemisión: si se pierde esta respuesta, hay que emitir otra
	// a mano desde su panel. El reintento devuelve vacío a propósito, porque
	// emitir una nueva obligaría a revocar la que quizá ya esté funcionando.
	SK string `json:"sk"`
}

// CreateStore crea la tienda de una organización.
//
// `idempotencyKey` es obligatoria y tiene que ser **estable por organización**
// (`bawto:org:<uuid>`): es lo único que impide que un timeout o un doble clic
// dejen dos tiendas, y dos tiendas con el mismo nombre —con el bot apuntando a
// la que no tiene el catálogo— es un fallo caro y difícil de ver.
func (c *Client) CreateStore(ctx context.Context, name, idempotencyKey string) (*Store, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("meudprov: la tienda necesita un nombre")
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		return nil, errors.New("meudprov: crear una tienda exige Idempotency-Key")
	}
	var store Store
	// El dueño no viaja en el cuerpo: lo fija MEUD desde la credencial. Si fuera
	// un parámetro, quien tuviera la clave podría crear tiendas a nombre de
	// cualquiera.
	err := c.do(ctx, request{
		method:         http.MethodPost,
		path:           "/stores",
		body:           map[string]string{"name": name},
		idempotencyKey: idempotencyKey,
	}, &store)
	if err != nil {
		return nil, err
	}
	if store.ID == 0 {
		return nil, &Error{Message: "MEUD no devolvió el id de la tienda"}
	}
	return &store, nil
}

// AddAdmin da acceso al cliente a su tienda en meud.im, con rol `admin`.
//
// `already` es el 409: ya era miembro, así que el estado deseado se cumple igual
// y no es un fallo. El 404 —no tiene cuenta en meud.im todavía— sí vuelve como
// error, porque exige decirle algo al cliente; su mensaje ya viene redactado
// para mostrárselo tal cual.
func (c *Client) AddAdmin(ctx context.Context, storeID int64, email string) (already bool, err error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return false, errors.New("meudprov: falta el correo")
	}
	if storeID <= 0 {
		return false, errors.New("meudprov: falta la tienda")
	}
	err = c.do(ctx, request{
		method: http.MethodPost,
		path:   fmt.Sprintf("/stores/%d/admins", storeID),
		body:   map[string]string{"email": email},
	}, nil)
	var apiErr *Error
	if errors.As(err, &apiErr) && apiErr.Conflict() {
		return true, nil
	}
	return false, err
}

type request struct {
	method         string
	path           string
	body           any
	idempotencyKey string
}

type envelope struct {
	OK   bool            `json:"ok"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// do ejecuta la petición y reintenta **una vez** cuando repetir es seguro.
//
// Crear una tienda lleva Idempotency-Key, así que el reintento devuelve la
// tienda que ya se creó en vez de una segunda. Añadir un administrador no la
// lleva, pero repetirlo responde 409 —«ya es miembro»—, que se trata como éxito;
// el efecto es el mismo.
func (c *Client) do(ctx context.Context, r request, out any) error {
	err := c.attempt(ctx, r, out)
	if err == nil {
		return nil
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || !apiErr.Retryable() {
		return err
	}
	return c.attempt(ctx, r, out)
}

func (c *Client) attempt(ctx context.Context, r request, out any) error {
	var payload io.Reader
	if r.body != nil {
		raw, err := json.Marshal(r.body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, c.baseURL+r.path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("X-Provision-Key", c.key)
	req.Header.Set("Accept", "application/json")
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", r.idempotencyKey)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return &Error{Unreachable: true, Message: err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return &Error{Unreachable: true, Message: err.Error()}
	}
	var env envelope
	decodeErr := json.Unmarshal(raw, &env)

	if resp.StatusCode >= 300 || (decodeErr == nil && !env.OK) {
		message := strings.TrimSpace(env.Msg)
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return &Error{StatusCode: resp.StatusCode, Message: message}
	}
	if decodeErr != nil {
		return &Error{StatusCode: resp.StatusCode, Message: "respuesta ilegible: " + decodeErr.Error()}
	}
	if out == nil || len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return &Error{StatusCode: resp.StatusCode, Message: "datos ilegibles: " + err.Error()}
	}
	return nil
}
