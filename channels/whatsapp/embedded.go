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
