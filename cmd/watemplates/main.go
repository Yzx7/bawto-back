// Lista las plantillas de las WABA a las que alcanza el token de un bot y permite
// crear, de forma idempotente, las plantillas iniciales de recordatorio ISP.
//
//	go run ./cmd/watemplates <botId>
//	go run ./cmd/watemplates <botId> create-isp-reminders <wabaId>
//	go run ./cmd/watemplates <botId> sync-catalog <wabaId>
//
// Sirve como herramienta operativa para crear las plantillas iniciales,
// consultar Meta y sincronizar el catálogo local sin requerir una sesión web.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/Yzx7/sacs-chatbots/channels/whatsapp"
	"github.com/Yzx7/sacs-chatbots/helpers"
	"github.com/Yzx7/sacs-chatbots/models"
)

type templateComponent struct {
	Type    string          `json:"type"`
	Text    string          `json:"text,omitempty"`
	Example json.RawMessage `json:"example,omitempty"`
}

type templateDefinition struct {
	Name       string              `json:"name"`
	Language   string              `json:"language"`
	Category   string              `json:"category"`
	Components []templateComponent `json:"components"`
}

type templateInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Language   string `json:"language"`
	Status     string `json:"status"`
	Category   string `json:"category"`
	Components []struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		Format string `json:"format"`
	} `json:"components"`
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func get(ctx context.Context, raw string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

func postJSON(ctx context.Context, raw, token string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, raw, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	responseBody, _ := io.ReadAll(res.Body)
	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	if out == nil || len(responseBody) == 0 {
		return nil
	}
	return json.Unmarshal(responseBody, out)
}

func ispReminderTemplates() []templateDefinition {
	examples := []byte(`{"body_text":[["María","F-2026-001","S/ 89.90","31/07/2026"]]}`)
	return []templateDefinition{
		{
			Name:     "recordatorio_pago_proximo_v1",
			Language: "es",
			Category: "UTILITY",
			Components: []templateComponent{{
				Type:    "BODY",
				Text:    "Hola, {{1}}. Te recordamos que el recibo {{2}} de tu servicio de internet, por {{3}}, vence el {{4}}.\n\nSi ya realizaste el pago, responde a este mensaje enviando tu comprobante.",
				Example: examples,
			}},
		},
		{
			Name:     "recordatorio_pago_hoy_v1",
			Language: "es",
			Category: "UTILITY",
			Components: []templateComponent{{
				Type:    "BODY",
				Text:    "Hola, {{1}}. El recibo {{2}} de tu servicio de internet, por {{3}}, vence hoy, {{4}}.\n\nSi ya realizaste el pago, responde a este mensaje enviando tu comprobante.",
				Example: examples,
			}},
		},
		{
			Name:     "recordatorio_pago_vencido_v1",
			Language: "es",
			Category: "UTILITY",
			Components: []templateComponent{{
				Type:    "BODY",
				Text:    "Hola, {{1}}. El recibo {{2}} de tu servicio de internet, por {{3}}, venció el {{4}} y continúa pendiente.\n\nSi ya realizaste el pago, responde a este mensaje enviando tu comprobante.",
				Example: examples,
			}},
		},
	}
}

func templateKey(name, language string) string {
	return name + "\x00" + language
}

func createISPReminders(ctx context.Context, base, version, token, wabaID, channelID string) error {
	var account struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	}
	if err := get(ctx, fmt.Sprintf("%s/%s/%s?fields=name&access_token=%s",
		base, version, wabaID, url.QueryEscape(token)), &account); err != nil {
		return fmt.Errorf("consultar WABA: %w", err)
	}

	var phones struct {
		Data []struct {
			ID                 string `json:"id"`
			DisplayPhoneNumber string `json:"display_phone_number"`
			VerifiedName       string `json:"verified_name"`
		} `json:"data"`
	}
	if err := get(ctx, fmt.Sprintf("%s/%s/%s/phone_numbers?fields=id,display_phone_number,verified_name&access_token=%s",
		base, version, wabaID, url.QueryEscape(token)), &phones); err != nil {
		return fmt.Errorf("consultar números del WABA: %w", err)
	}
	var matchedPhone string
	for _, phone := range phones.Data {
		if phone.ID == channelID {
			matchedPhone = phone.DisplayPhoneNumber
			break
		}
	}
	if matchedPhone == "" {
		return fmt.Errorf("el WABA %s (%s) no contiene el phone_number_id %s del bot", wabaID, account.Name, channelID)
	}
	fmt.Printf("Destino verificado: WABA %s (%s), número %s, phone_number_id=%s\n",
		wabaID, account.Name, matchedPhone, channelID)

	var current struct {
		Data []templateInfo `json:"data"`
	}
	if err := get(ctx, fmt.Sprintf("%s/%s/%s/message_templates?fields=id,name,language,status,category,components&limit=200&access_token=%s",
		base, version, wabaID, url.QueryEscape(token)), &current); err != nil {
		return fmt.Errorf("consultar plantillas existentes: %w", err)
	}
	existing := make(map[string]templateInfo, len(current.Data))
	for _, tpl := range current.Data {
		existing[templateKey(tpl.Name, tpl.Language)] = tpl
	}

	for _, definition := range ispReminderTemplates() {
		if tpl, ok := existing[templateKey(definition.Name, definition.Language)]; ok {
			fmt.Printf("OMITIDA  %-34s id=%s status=%s category=%s\n",
				tpl.Name, tpl.ID, tpl.Status, tpl.Category)
			continue
		}
		var created struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			Category string `json:"category"`
		}
		endpoint := fmt.Sprintf("%s/%s/%s/message_templates", base, version, wabaID)
		if err := postJSON(ctx, endpoint, token, definition, &created); err != nil {
			return fmt.Errorf("crear %s: %w", definition.Name, err)
		}
		fmt.Printf("CREADA   %-34s id=%s status=%s category=%s\n",
			definition.Name, created.ID, created.Status, created.Category)
	}
	return nil
}

func main() {
	_ = godotenv.Load()
	if len(os.Args) != 2 && len(os.Args) != 4 {
		fmt.Fprintf(os.Stderr, "uso: %s <botId> [create-isp-reminders|sync-catalog <wabaId>]\n", os.Args[0])
		os.Exit(2)
	}
	botID := os.Args[1]
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		fmt.Println("ERR pool:", err)
		os.Exit(1)
	}
	defer pool.Close()

	var enc []byte
	var chid string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(channel_id,''), token_enc FROM bots WHERE id=$1::uuid`, botID).Scan(&chid, &enc); err != nil {
		fmt.Println("ERR bot:", err)
		os.Exit(1)
	}
	cph, err := helpers.NewCipher(os.Getenv("TOKEN_ENC_KEY"))
	if err != nil {
		fmt.Println("ERR cipher:", err)
		os.Exit(1)
	}
	token, err := cph.Decrypt(enc)
	if err != nil {
		fmt.Println("ERR decrypt:", err)
		os.Exit(1)
	}

	base := env("WHATSAPP_API_BASE", "https://graph.facebook.com")
	ver := env("WHATSAPP_API_VERSION", "v21.0")
	appToken := os.Getenv("FACEBOOK_APP_ID") + "|" + os.Getenv("WHATSAPP_APP_SECRET")

	// El WABA no está persistido: se deduce de los permisos granulares del token.
	var dbg struct {
		Data struct {
			GranularScopes []struct {
				Scope     string   `json:"scope"`
				TargetIDs []string `json:"target_ids"`
			} `json:"granular_scopes"`
			ExpiresAt int64 `json:"expires_at"`
			Type      string
			AppID     string `json:"app_id"`
		} `json:"data"`
	}
	dbgURL := fmt.Sprintf("%s/%s/debug_token?input_token=%s&access_token=%s",
		base, ver, url.QueryEscape(token), url.QueryEscape(appToken))
	if err := get(ctx, dbgURL, &dbg); err != nil {
		fmt.Println("ERR debug_token:", err)
		os.Exit(1)
	}
	exp := "sin caducidad"
	if dbg.Data.ExpiresAt > 0 {
		exp = time.Unix(dbg.Data.ExpiresAt, 0).Format("2006-01-02")
	}
	fmt.Printf("Bot %s · phone_number_id=%s · token tipo=%s caduca=%s\n\n", botID, chid, dbg.Data.Type, exp)

	fmt.Println("Permisos granulares del token:")
	for _, s := range dbg.Data.GranularScopes {
		fmt.Printf("  %-40s %v\n", s.Scope, s.TargetIDs)
	}
	fmt.Println()

	wabas := map[string]bool{}
	for _, s := range dbg.Data.GranularScopes {
		if strings.HasPrefix(s.Scope, "whatsapp_business_") {
			for _, id := range s.TargetIDs {
				wabas[id] = true
			}
		}
	}
	if len(wabas) == 0 {
		fmt.Println("El token no declara ninguna WABA en sus permisos granulares.")
		return
	}
	if len(os.Args) == 4 {
		action := os.Args[2]
		if action != "create-isp-reminders" && action != "sync-catalog" {
			fmt.Fprintln(os.Stderr, "acción desconocida:", os.Args[2])
			os.Exit(2)
		}
		targetWABA := os.Args[3]
		if !wabas[targetWABA] {
			fmt.Fprintf(os.Stderr, "el token del bot no declara acceso al WABA %s\n", targetWABA)
			os.Exit(1)
		}
		if action == "create-isp-reminders" {
			if err := createISPReminders(ctx, base, ver, token, targetWABA, chid); err != nil {
				fmt.Fprintln(os.Stderr, "ERR create-isp-reminders:", err)
				os.Exit(1)
			}
			return
		}

		items, err := whatsapp.ListTemplates(ctx, whatsapp.SendConfig{
			APIBase: base,
			Version: ver,
			Token:   token,
		}, targetWABA)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERR consultar catálogo en Meta:", err)
			os.Exit(1)
		}
		report, err := models.SyncChannelTemplates(ctx, pool, targetWABA, items, time.Now().UTC())
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERR sincronizar catálogo local:", err)
			os.Exit(1)
		}
		fmt.Printf("Catálogo sincronizado: total=%d marcadas_eliminadas=%d fecha=%s\n",
			report.Total, report.MarkedDeleted, report.SyncedAt.Format(time.RFC3339))
		return
	}

	ids := make([]string, 0, len(wabas))
	for id := range wabas {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, waba := range ids {
		var acc struct {
			Name string `json:"name"`
			ID   string `json:"id"`
		}
		_ = get(ctx, fmt.Sprintf("%s/%s/%s?fields=name&access_token=%s", base, ver, waba, url.QueryEscape(token)), &acc)
		fmt.Printf("=== WABA %s  %s ===\n", waba, acc.Name)

		var tpl struct {
			Data []templateInfo `json:"data"`
		}
		u := fmt.Sprintf("%s/%s/%s/message_templates?fields=name,language,status,category,components&limit=200&access_token=%s",
			base, ver, waba, url.QueryEscape(token))
		if err := get(ctx, u, &tpl); err != nil {
			fmt.Println("  ERR message_templates:", err)
			continue
		}
		if len(tpl.Data) == 0 {
			fmt.Println("  (sin plantillas)")
			continue
		}
		for _, t := range tpl.Data {
			fmt.Printf("  · %-28s %-6s %-10s %-10s\n", t.Name, t.Language, t.Status, t.Category)
			for _, c := range t.Components {
				txt := strings.ReplaceAll(c.Text, "\n", " ⏎ ")
				if len(txt) > 150 {
					txt = txt[:150] + "…"
				}
				if txt != "" {
					fmt.Printf("      %-8s %s\n", strings.ToLower(c.Type), txt)
				}
			}
		}
		fmt.Println()
	}
}
