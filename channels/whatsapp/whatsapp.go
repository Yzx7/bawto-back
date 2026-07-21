package whatsapp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Yzx7/sacs-chatbots/channels"
)

// Channel es el identificador de canal en la BD.
const Channel = "wsp"

// VerifyChallenge valida el handshake del webhook (GET) y devuelve el challenge.
func VerifyChallenge(mode, verifyToken, challenge, expected string) (string, bool) {
	if mode == "subscribe" && expected != "" && verifyToken == expected {
		return challenge, true
	}
	return "", false
}

// CheckSignature valida X-Hub-Signature-256 (HMAC-SHA256 del body con el app secret).
func CheckSignature(appSecret string, body []byte, header string) bool {
	if appSecret == "" || header == "" {
		return false
	}
	sig := strings.TrimPrefix(header, "sha256=")
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sig))
}

// Sign devuelve el header X-Hub-Signature-256 para un body (útil para tests/simulación).
func Sign(appSecret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// ---- Parse ----

type webhookPayload struct {
	Entry []struct {
		Changes []struct {
			Value struct {
				Metadata struct {
					PhoneNumberID string `json:"phone_number_id"`
				} `json:"metadata"`
				Contacts []struct {
					Profile struct {
						Name string `json:"name"`
					} `json:"profile"`
					WaID string `json:"wa_id"`
				} `json:"contacts"`
				Messages []struct {
					From string `json:"from"`
					ID   string `json:"id"`
					Type string `json:"type"`
					Text struct {
						Body string `json:"body"`
					} `json:"text"`
					Image struct {
						ID       string `json:"id"`
						MimeType string `json:"mime_type"`
						Caption  string `json:"caption"`
					} `json:"image"`
					Interactive struct {
						ButtonReply struct {
							ID    string `json:"id"`
							Title string `json:"title"`
						} `json:"button_reply"`
						ListReply struct {
							ID    string `json:"id"`
							Title string `json:"title"`
						} `json:"list_reply"`
					} `json:"interactive"`
				} `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// Parse normaliza un payload del webhook de WhatsApp a InboundMessages.
func Parse(body []byte) ([]channels.InboundMessage, error) {
	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	var out []channels.InboundMessage
	for _, e := range p.Entry {
		for _, ch := range e.Changes {
			v := ch.Value
			name := ""
			if len(v.Contacts) > 0 {
				name = v.Contacts[0].Profile.Name
			}
			for _, m := range v.Messages {
				im := channels.InboundMessage{
					ChannelID:   v.Metadata.PhoneNumberID,
					WaID:        m.ID,
					From:        m.From,
					ContactName: name,
				}
				switch m.Type {
				case "text":
					im.Type = channels.MsgText
					im.Text = m.Text.Body
				case "interactive":
					im.Type = channels.MsgReply
					if m.Interactive.ButtonReply.ID != "" {
						im.ReplyID = m.Interactive.ButtonReply.ID
						im.Text = m.Interactive.ButtonReply.Title
					} else {
						im.ReplyID = m.Interactive.ListReply.ID
						im.Text = m.Interactive.ListReply.Title
					}
				case "image":
					im.Type = channels.MsgImage
					im.MediaID = m.Image.ID
					im.MimeType = m.Image.MimeType
					im.Caption = m.Image.Caption
					im.Text = m.Image.Caption
				default:
					im.Type = channels.MsgOther
				}
				out = append(out, im)
			}
		}
	}
	return out, nil
}

// ---- Ecos (Coexistence) ----

// Echo es un mensaje saliente del negocio. Con Coexistence, Meta envía por
// `smb_message_echoes` también los que escribe una persona desde la app de
// WhatsApp Business: así detectamos que un humano tomó la conversación.
type Echo struct {
	ChannelID string // phone_number_id del negocio
	WaID      string // id del mensaje
	To        string // contacto destino
	Type      string
	Text      string
}

type echoPayload struct {
	Entry []struct {
		Changes []struct {
			Field string `json:"field"`
			Value struct {
				Metadata struct {
					PhoneNumberID string `json:"phone_number_id"`
				} `json:"metadata"`
				MessageEchoes []struct {
					To   string `json:"to"`
					ID   string `json:"id"`
					Type string `json:"type"`
					Text struct {
						Body string `json:"body"`
					} `json:"text"`
				} `json:"message_echoes"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// ParseEchoes extrae los ecos de mensajes salientes del negocio (message_echoes).
func ParseEchoes(body []byte) ([]Echo, error) {
	var p echoPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	var out []Echo
	for _, e := range p.Entry {
		for _, ch := range e.Changes {
			for _, m := range ch.Value.MessageEchoes {
				out = append(out, Echo{
					ChannelID: ch.Value.Metadata.PhoneNumberID,
					WaID:      m.ID,
					To:        m.To,
					Type:      m.Type,
					Text:      m.Text.Body,
				})
			}
		}
	}
	return out, nil
}

// ---- Send (Cloud API) ----

type SendConfig struct {
	APIBase       string // https://graph.facebook.com
	Version       string // v21.0
	PhoneNumberID string
	Token         string // access token (descifrado)
	HTTP          *http.Client
}

// DownloadMedia resuelve el media_id y guarda una copia durable antes de que
// expire la URL temporal de Meta. Limita cada archivo a 10 MiB.
func DownloadMedia(ctx context.Context, cfg SendConfig, mediaID string) ([]byte, string, error) {
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	metaURL := fmt.Sprintf("%s/%s/%s", strings.TrimRight(cfg.APIBase, "/"), cfg.Version, mediaID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("whatsapp media info %d", resp.StatusCode)
	}
	var info struct {
		URL      string `json:"url"`
		MimeType string `json:"mime_type"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return nil, "", err
	}
	parsed, err := url.Parse(info.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, "", fmt.Errorf("URL de media inválida")
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, info.URL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err = hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("whatsapp media download %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (10<<20)+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > 10<<20 {
		return nil, "", fmt.Errorf("la imagen supera 10 MiB")
	}
	if info.MimeType == "" {
		info.MimeType = resp.Header.Get("Content-Type")
	}
	return data, info.MimeType, nil
}

// PhoneInfo es la información del número conectado que exponemos al panel.
type PhoneInfo struct {
	PhoneNumberID      string `json:"phoneNumberId"`
	DisplayPhoneNumber string `json:"displayPhoneNumber"`
	VerifiedName       string `json:"verifiedName"`
	NameStatus         string `json:"nameStatus"`
	QualityRating      string `json:"qualityRating"`
	PlatformType       string `json:"platformType"`
	CodeVerification   string `json:"codeVerification"`
	Throughput         string `json:"throughput"`
}

// GetPhoneInfo consulta en la Cloud API los datos del número conectado.
func GetPhoneInfo(ctx context.Context, cfg SendConfig) (*PhoneInfo, error) {
	const fields = "display_phone_number,verified_name,name_status,quality_rating,platform_type,code_verification_status,throughput"
	url := fmt.Sprintf("%s/%s/%s?fields=%s", strings.TrimRight(cfg.APIBase, "/"), cfg.Version, cfg.PhoneNumberID, fields)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("whatsapp phone info %d: %s", resp.StatusCode, string(rb))
	}

	var m struct {
		ID                     string `json:"id"`
		DisplayPhoneNumber     string `json:"display_phone_number"`
		VerifiedName           string `json:"verified_name"`
		NameStatus             string `json:"name_status"`
		QualityRating          string `json:"quality_rating"`
		PlatformType           string `json:"platform_type"`
		CodeVerificationStatus string `json:"code_verification_status"`
		Throughput             struct {
			Level string `json:"level"`
		} `json:"throughput"`
	}
	if err := json.Unmarshal(rb, &m); err != nil {
		return nil, err
	}
	return &PhoneInfo{
		PhoneNumberID:      m.ID,
		DisplayPhoneNumber: m.DisplayPhoneNumber,
		VerifiedName:       m.VerifiedName,
		NameStatus:         m.NameStatus,
		QualityRating:      m.QualityRating,
		PlatformType:       m.PlatformType,
		CodeVerification:   m.CodeVerificationStatus,
		Throughput:         m.Throughput.Level,
	}, nil
}

// SendText envía un mensaje de texto por la Cloud API y devuelve el id del mensaje.
func SendText(ctx context.Context, cfg SendConfig, to, body string) (string, error) {
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "text",
		"text":              map[string]string{"body": body},
	}
	buf, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/%s/%s/messages", strings.TrimRight(cfg.APIBase, "/"), cfg.Version, cfg.PhoneNumberID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("whatsapp send %d: %s", resp.StatusCode, string(rb))
	}
	var r struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(rb, &r)
	if len(r.Messages) > 0 {
		return r.Messages[0].ID, nil
	}
	return "", nil
}

// MarkMessageAsRead confirma a WhatsApp que el mensaje entrante fue leído.
func MarkMessageAsRead(ctx context.Context, cfg SendConfig, messageID string) error {
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"status":            "read",
		"message_id":        messageID,
	}
	return sendStatusPayload(ctx, cfg, payload)
}

// ShowTypingIndicator marca el mensaje como leído y muestra "escribiendo...".
// WhatsApp oculta el indicador cuando se envía la respuesta o tras 25 segundos.
func ShowTypingIndicator(ctx context.Context, cfg SendConfig, messageID string) error {
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"status":            "read",
		"message_id":        messageID,
		"typing_indicator": map[string]string{
			"type": "text",
		},
	}
	return sendStatusPayload(ctx, cfg, payload)
}

func sendStatusPayload(ctx context.Context, cfg SendConfig, payload map[string]any) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/%s/%s/messages", strings.TrimRight(cfg.APIBase, "/"), cfg.Version, cfg.PhoneNumberID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("whatsapp message status %d: %s", resp.StatusCode, string(rb))
	}
	return nil
}

// SendTemplate inicia una conversación proactiva con una plantilla aprobada.
func SendTemplate(ctx context.Context, cfg SendConfig, to, name, language string, params []string) (string, error) {
	parameters := make([]map[string]string, len(params))
	for i, p := range params {
		parameters[i] = map[string]string{"type": "text", "text": p}
	}
	template := map[string]any{"name": name, "language": map[string]string{"code": language}}
	if len(parameters) > 0 {
		template["components"] = []any{map[string]any{"type": "body", "parameters": parameters}}
	}
	payload := map[string]any{"messaging_product": "whatsapp", "to": to, "type": "template", "template": template}
	return sendPayload(ctx, cfg, payload)
}

func sendPayload(ctx context.Context, cfg SendConfig, payload map[string]any) (string, error) {
	buf, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/%s/%s/messages", strings.TrimRight(cfg.APIBase, "/"), cfg.Version, cfg.PhoneNumberID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("whatsapp send %d: %s", resp.StatusCode, string(rb))
	}
	var r struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(rb, &r)
	if len(r.Messages) > 0 {
		return r.Messages[0].ID, nil
	}
	return "", nil
}
