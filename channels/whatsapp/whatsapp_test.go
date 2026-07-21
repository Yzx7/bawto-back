package whatsapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Yzx7/sacs-chatbots/channels"
)

func TestSignature(t *testing.T) {
	secret := "appsecret"
	body := []byte(`{"a":1}`)
	h := Sign(secret, body)
	if !CheckSignature(secret, body, h) {
		t.Fatal("firma válida rechazada")
	}
	if CheckSignature(secret, body, "sha256=deadbeef") {
		t.Fatal("firma inválida aceptada")
	}
	if CheckSignature("", body, h) {
		t.Fatal("sin app secret debe fallar")
	}
}

func TestVerifyChallenge(t *testing.T) {
	ch, ok := VerifyChallenge("subscribe", "tok", "1234", "tok")
	if !ok || ch != "1234" {
		t.Fatalf("challenge esperado 1234/ok, got %q/%v", ch, ok)
	}
	if _, ok := VerifyChallenge("subscribe", "malo", "1234", "tok"); ok {
		t.Fatal("verify token incorrecto fue aceptado")
	}
}

func TestParseText(t *testing.T) {
	payload := `{"entry":[{"changes":[{"value":{"metadata":{"phone_number_id":"PNID"},"contacts":[{"profile":{"name":"Ana"},"wa_id":"51999"}],"messages":[{"from":"51999","id":"wamid.1","type":"text","text":{"body":"hola"}}]}}]}]}`
	msgs, err := Parse([]byte(payload))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("esperaba 1 mensaje, got %d", len(msgs))
	}
	m := msgs[0]
	if m.ChannelID != "PNID" || m.From != "51999" || m.WaID != "wamid.1" ||
		m.Text != "hola" || m.ContactName != "Ana" || m.Type != channels.MsgText {
		t.Fatalf("mensaje mal parseado: %+v", m)
	}
}

func TestParseImageKeepsMediaMetadata(t *testing.T) {
	payload := `{"entry":[{"changes":[{"value":{"metadata":{"phone_number_id":"PNID"},"contacts":[{"profile":{"name":"Ana"}}],"messages":[{"from":"51999","id":"wamid.img","type":"image","image":{"id":"media-1","mime_type":"image/jpeg","caption":"voucher"}}]}}]}]}`
	msgs, err := Parse([]byte(payload))
	if err != nil || len(msgs) != 1 {
		t.Fatalf("Parse: len=%d err=%v", len(msgs), err)
	}
	msg := msgs[0]
	if msg.Type != channels.MsgImage || msg.MediaID != "media-1" || msg.MimeType != "image/jpeg" || msg.Text != "voucher" {
		t.Fatalf("imagen mal normalizada: %+v", msg)
	}
}

func TestSendText(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.out"}]}`))
	}))
	defer srv.Close()

	id, err := SendText(context.Background(), SendConfig{
		APIBase: srv.URL, Version: "v21.0", PhoneNumberID: "PNID", Token: "TKN",
	}, "51999", "hey")
	if err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if id != "wamid.out" {
		t.Fatalf("id esperado wamid.out, got %q", id)
	}
	if gotPath != "/v21.0/PNID/messages" {
		t.Fatalf("path incorrecto: %q", gotPath)
	}
	if gotAuth != "Bearer TKN" {
		t.Fatalf("auth incorrecto: %q", gotAuth)
	}
	if gotBody["to"] != "51999" {
		t.Fatalf("body incorrecto: %v", gotBody)
	}
}

func TestMessageReadAndTypingPayloads(t *testing.T) {
	tests := []struct {
		name       string
		send       func(context.Context, SendConfig, string) error
		wantTyping bool
	}{
		{name: "read", send: MarkMessageAsRead},
		{name: "typing", send: ShowTypingIndicator, wantTyping: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath, gotAuth string
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &gotBody)
				_, _ = w.Write([]byte(`{"success":true}`))
			}))
			defer srv.Close()

			err := tt.send(context.Background(), SendConfig{
				APIBase: srv.URL, Version: "v21.0", PhoneNumberID: "PNID", Token: "TKN",
			}, "wamid.in")
			if err != nil {
				t.Fatalf("status request: %v", err)
			}
			if gotPath != "/v21.0/PNID/messages" || gotAuth != "Bearer TKN" {
				t.Fatalf("request incorrecto: path=%q auth=%q", gotPath, gotAuth)
			}
			if gotBody["messaging_product"] != "whatsapp" || gotBody["status"] != "read" || gotBody["message_id"] != "wamid.in" {
				t.Fatalf("payload incorrecto: %v", gotBody)
			}
			_, hasTyping := gotBody["typing_indicator"]
			if hasTyping != tt.wantTyping {
				t.Fatalf("typing_indicator=%v, esperado=%v; payload=%v", hasTyping, tt.wantTyping, gotBody)
			}
			if tt.wantTyping {
				typing, ok := gotBody["typing_indicator"].(map[string]any)
				if !ok || typing["type"] != "text" {
					t.Fatalf("typing_indicator incorrecto: %v", gotBody["typing_indicator"])
				}
			}
		})
	}
}

func TestDownloadMedia(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer TKN" {
			t.Fatalf("auth ausente: %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path == "/v21.0/media-1" {
			_ = json.NewEncoder(w).Encode(map[string]string{"url": srv.URL + "/file", "mime_type": "image/jpeg"})
			return
		}
		_, _ = w.Write([]byte("image-bytes"))
	}))
	defer srv.Close()
	data, mimeType, err := DownloadMedia(context.Background(), SendConfig{APIBase: srv.URL, Version: "v21.0", Token: "TKN", HTTP: srv.Client()}, "media-1")
	if err != nil || string(data) != "image-bytes" || mimeType != "image/jpeg" {
		t.Fatalf("DownloadMedia: data=%q mime=%q err=%v", data, mimeType, err)
	}
}
