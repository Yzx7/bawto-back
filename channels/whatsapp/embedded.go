package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RegisterPhone registra un numero en Cloud API y establece su PIN de
// verificacion en dos pasos en la misma operacion.
func RegisterPhone(ctx context.Context, apiBase, version, phoneNumberID, token, pin string, hc *http.Client) error {
	payload, err := json.Marshal(map[string]string{
		"messaging_product": "whatsapp",
		"pin":               pin,
	})
	if err != nil {
		return err
	}

	u := fmt.Sprintf("%s/%s/%s/register", strings.TrimRight(apiBase, "/"), version, phoneNumberID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient(hc).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("register phone %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func httpClient(hc *http.Client) *http.Client {
	if hc != nil {
		return hc
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// ExchangeCode intercambia el `code` del Embedded Signup por un access token largo
// (GET /{version}/oauth/access_token con App ID + App Secret).
func ExchangeCode(ctx context.Context, apiBase, version, appID, appSecret, code string, hc *http.Client) (string, error) {
	q := url.Values{}
	q.Set("client_id", appID)
	q.Set("client_secret", appSecret)
	q.Set("code", code)
	u := fmt.Sprintf("%s/%s/oauth/access_token?%s", strings.TrimRight(apiBase, "/"), version, q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := httpClient(hc).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("exchange code %d: %s", resp.StatusCode, string(body))
	}
	var r struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", err
	}
	if r.AccessToken == "" {
		return "", fmt.Errorf("exchange code: sin access_token en la respuesta")
	}
	return r.AccessToken, nil
}

// SubscribeWABA suscribe nuestra app a la WABA del cliente para recibir sus webhooks
// (POST /{version}/{waba_id}/subscribed_apps con el token del cliente).
func SubscribeWABA(ctx context.Context, apiBase, version, wabaID, token string, hc *http.Client) error {
	u := fmt.Sprintf("%s/%s/%s/subscribed_apps", strings.TrimRight(apiBase, "/"), version, wabaID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient(hc).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("subscribe waba %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// DiscoverWABAs deduce a que WABA da acceso el token recien intercambiado.
//
// Existe porque los ids del Embedded Signup no viajan en el `code`: el popup los
// manda por postMessage al panel. En un navegador movil ese postMessage no llega
// —la ventana de m.facebook.com no conserva el opener—, asi que el cliente
// termina el flujo en Meta y el panel se queda sin `phone_number_id` ni
// `waba_id`. Antes eso descartaba un `code` perfectamente valido, que caduca en
// 30 s, y la conexion se perdia sin dejar rastro en el backend.
//
// El token de ES trae los ids en sus granular_scopes, que es el camino que Meta
// documenta para este caso.
func DiscoverWABAs(ctx context.Context, apiBase, version, appID, appSecret, token string, hc *http.Client) ([]string, error) {
	q := url.Values{}
	q.Set("input_token", token)
	q.Set("access_token", appID+"|"+appSecret)
	u := fmt.Sprintf("%s/%s/debug_token?%s", strings.TrimRight(apiBase, "/"), version, q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient(hc).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("debug_token %d: %s", resp.StatusCode, string(body))
	}
	var r struct {
		Data struct {
			GranularScopes []struct {
				Scope     string   `json:"scope"`
				TargetIDs []string `json:"target_ids"`
			} `json:"granular_scopes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	// whatsapp_business_management es el scope que enumera las WABA; el de
	// messaging apunta a los numeros y no sirve para identificar la cuenta.
	seen := map[string]bool{}
	var out []string
	for _, s := range r.Data.GranularScopes {
		if s.Scope != "whatsapp_business_management" {
			continue
		}
		for _, id := range s.TargetIDs {
			if id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out, nil
}

// PhoneNumber es un numero de una WABA tal como lo devuelve Meta.
type PhoneNumber struct {
	ID                 string `json:"id"`
	DisplayPhoneNumber string `json:"display_phone_number"`
	VerifiedName       string `json:"verified_name"`
}

// ListPhoneNumbers lista los numeros de una WABA. Completa a DiscoverWABAs
// cuando el panel no recibio el phone_number_id por postMessage.
func ListPhoneNumbers(ctx context.Context, apiBase, version, wabaID, token string, hc *http.Client) ([]PhoneNumber, error) {
	u := fmt.Sprintf("%s/%s/%s/phone_numbers", strings.TrimRight(apiBase, "/"), version, wabaID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient(hc).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("phone_numbers %d: %s", resp.StatusCode, string(body))
	}
	var r struct {
		Data []PhoneNumber `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	return r.Data, nil
}
