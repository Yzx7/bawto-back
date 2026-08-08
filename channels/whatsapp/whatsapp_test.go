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
	payload := `{"entry":[{"changes":[{"value":{"metadata":{"phone_number_id":"PNID"},"contacts":[{"profile":{"name":"Ana"},"wa_id":"51999"}],"messages":[{"from":"51999","id":"wamid.1","type":"text","context":{"id":"wamid.out"},"text":{"body":"hola"}}]}}]}]}`
	msgs, err := Parse([]byte(payload))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("esperaba 1 mensaje, got %d", len(msgs))
	}
	m := msgs[0]
	if m.ChannelID != "PNID" || m.From != "51999" || m.WaID != "wamid.1" ||
		m.Text != "hola" || m.ContactName != "Ana" || m.Type != channels.MsgText ||
		m.QuotedWaID != "wamid.out" {
		t.Fatalf("mensaje mal parseado: %+v", m)
	}
}

func TestParseStatuses(t *testing.T) {
	payload := `{"entry":[{"changes":[{"value":{"metadata":{"phone_number_id":"PNID"},"statuses":[{"id":"wamid.out","status":"failed","timestamp":"1785283200","recipient_id":"51999","biz_opaque_callback_data":"run-123","conversation":{"id":"conv-1","origin":{"type":"utility"}},"pricing":{"billable":true,"pricing_model":"PMP","type":"regular","category":"utility"},"errors":[{"code":131026,"title":"Undeliverable","message":"Message undeliverable","error_data":{"details":"phone cannot receive"}}]}]}}]}]}`
	statuses, err := ParseStatuses([]byte(payload))
	if err != nil || len(statuses) != 1 {
		t.Fatalf("ParseStatuses: len=%d err=%v", len(statuses), err)
	}
	status := statuses[0]
	if status.ChannelID != "PNID" || status.MessageID != "wamid.out" ||
		status.Status != "failed" || status.ErrorCode != "131026" ||
		status.ErrorDetails != "phone cannot receive" || status.ConversationID != "conv-1" ||
		status.PricingModel != "PMP" || status.PricingType != "regular" ||
		status.PricingCategory != "utility" || status.OpaqueCallback != "run-123" ||
		status.Billable == nil || !*status.Billable {
		t.Fatalf("status mal parseado: %+v", status)
	}
}

func TestParseImageKeepsMediaMetadata(t *testing.T) {
	payload := `{"entry":[{"changes":[{"value":{"metadata":{"phone_number_id":"PNID"},"contacts":[{"profile":{"name":"Ana"}}],"messages":[{"from":"51999","id":"wamid.img","type":"image","context":{"forwarded":true},"image":{"id":"media-1","mime_type":"image/jpeg","caption":"voucher","sha256":"hash","url":"https://media.example/image"}}]}}]}]}`
	msgs, err := Parse([]byte(payload))
	if err != nil || len(msgs) != 1 {
		t.Fatalf("Parse: len=%d err=%v", len(msgs), err)
	}
	msg := msgs[0]
	if msg.EventType != channels.EventMessage || msg.Type != channels.MsgImage ||
		msg.MediaID != "media-1" || msg.MimeType != "image/jpeg" ||
		msg.MediaSHA256 != "hash" || msg.MediaURL != "https://media.example/image" ||
		msg.Caption != "voucher" || msg.Text != "voucher" || !msg.Forwarded || !msg.HasMedia() {
		t.Fatalf("imagen mal normalizada: %+v", msg)
	}
}

func TestParseMultimodalMessages(t *testing.T) {
	payload := `{"entry":[{"changes":[{"value":{"metadata":{"phone_number_id":"PNID"},"messages":[
		{"from":"51999","id":"a1","type":"audio","audio":{"id":"media-a","mime_type":"audio/ogg","sha256":"ha","voice":true}},
		{"from":"51999","id":"d1","type":"document","document":{"id":"media-d","mime_type":"application/pdf","sha256":"hd","filename":"recibo.pdf","caption":"julio"}},
		{"from":"51999","id":"v1","type":"video","video":{"id":"media-v","mime_type":"video/mp4","caption":"prueba"}},
		{"from":"51999","id":"s1","type":"sticker","sticker":{"id":"media-s","mime_type":"image/webp","animated":true}},
		{"from":"51999","id":"l1","type":"location","location":{"latitude":-12.1,"longitude":-77.0,"name":"Local","address":"Lima"}},
		{"from":"51999","id":"c1","type":"contacts","contacts":[{"name":{"formatted_name":"Ana"}}]},
		{"from":"51999","id":"o1","type":"order","order":{"catalog_id":"cat-1"}},
		{"from":"51999","id":"i1","type":"interactive","interactive":{"button_reply":{"id":"pay","title":"Pagar"}}}
	]}}]}]}`

	msgs, err := Parse([]byte(payload))
	if err != nil || len(msgs) != 8 {
		t.Fatalf("Parse multimodal: len=%d err=%v", len(msgs), err)
	}
	if msgs[0].Type != channels.MsgAudio || !msgs[0].Voice || !msgs[0].HasMedia() {
		t.Fatalf("audio: %+v", msgs[0])
	}
	if msgs[1].Type != channels.MsgDocument || msgs[1].FileName != "recibo.pdf" || msgs[1].Caption != "julio" || msgs[1].Text != "julio" {
		t.Fatalf("documento: %+v", msgs[1])
	}
	if msgs[2].Type != channels.MsgVideo || msgs[2].Caption != "prueba" || !msgs[2].HasMedia() {
		t.Fatalf("video: %+v", msgs[2])
	}
	if msgs[3].Type != channels.MsgSticker || !msgs[3].Animated || !msgs[3].HasMedia() {
		t.Fatalf("sticker: %+v", msgs[3])
	}
	if msgs[4].Type != channels.MsgLocation || msgs[4].Location == nil || msgs[4].Location.Latitude == nil || *msgs[4].Location.Latitude != -12.1 {
		t.Fatalf("ubicación: %+v", msgs[4])
	}
	if msgs[5].Type != channels.MsgContacts || len(msgs[5].Contacts) == 0 {
		t.Fatalf("contactos: %+v", msgs[5])
	}
	if msgs[6].Type != channels.MsgOrder || len(msgs[6].Order) == 0 {
		t.Fatalf("pedido: %+v", msgs[6])
	}
	if msgs[7].Type != channels.MsgInteractive || msgs[7].ReplyID != "pay" || msgs[7].Text != "Pagar" {
		t.Fatalf("interactivo: %+v", msgs[7])
	}
}

func TestParseReactionAndRemoval(t *testing.T) {
	payload := `{"entry":[{"changes":[{"value":{"metadata":{"phone_number_id":"PNID"},"messages":[
		{"from":"51999","id":"r1","type":"reaction","reaction":{"message_id":"out-1","emoji":"👍"}},
		{"from":"51999","id":"r2","type":"reaction","reaction":{"message_id":"out-1"}}
	]}}]}]}`
	msgs, err := Parse([]byte(payload))
	if err != nil || len(msgs) != 2 {
		t.Fatalf("Parse reaction: len=%d err=%v", len(msgs), err)
	}
	if msgs[0].EventType != channels.EventReaction || msgs[0].Type != channels.MsgReaction ||
		msgs[0].ReactionMessageID != "out-1" || msgs[0].ReactionEmoji != "👍" || msgs[0].ReactionRemoved {
		t.Fatalf("reacción: %+v", msgs[0])
	}
	if !msgs[1].ReactionRemoved || msgs[1].ReactionEmoji != "" || msgs[1].ReactionMessageID != "out-1" {
		t.Fatalf("reacción eliminada: %+v", msgs[1])
	}
}

// TestParseConservaElCuerpoOriginal fija que `Raw` sea el fragmento tal como lo
// mandó Meta y no una reserialización de la struct. La diferencia solo se nota en
// los campos que el parser no declara —aquí `referral`, el de los anuncios
// Click-to-WhatsApp—, y es la que decide si una campaña se puede atribuir: el
// mensaje pasa una vez, y lo que no se guarde entonces no se recupera nunca.
func TestParseConservaElCuerpoOriginal(t *testing.T) {
	payload := `{"entry":[{"changes":[{"value":{"metadata":{"phone_number_id":"PNID"},"messages":[
		{"from":"51999","id":"wamid.ad","type":"text","text":{"body":"hola"},
		 "referral":{"source_url":"https://fb.me/x","source_id":"120210000","source_type":"ad",
		  "headline":"¿Respondes mensajes todo el día?","body":"Bawto contesta por ti",
		  "media_type":"image","image_url":"https://scontent/x.jpg","ctwa_clid":"CLID123"}}
	]}}]}]}`
	msgs, err := Parse([]byte(payload))
	if err != nil || len(msgs) != 1 {
		t.Fatalf("Parse: len=%d err=%v", len(msgs), err)
	}

	// Lo tipado sigue funcionando: este cambio no reinterpreta nada.
	if msgs[0].Text != "hola" || msgs[0].Type != channels.MsgText || msgs[0].WaID != "wamid.ad" {
		t.Fatalf("el mensaje dejó de parsearse bien: %+v", msgs[0])
	}

	var got struct {
		Referral map[string]any `json:"referral"`
	}
	if err := json.Unmarshal(msgs[0].Raw, &got); err != nil {
		t.Fatalf("Raw no es JSON válido: %v", err)
	}
	if got.Referral == nil {
		t.Fatal("el referral se perdió: Raw no conserva los campos que la struct no declara")
	}
	// Campo por campo: guardar el objeto a medias sería la misma fuga más tarde.
	for key, want := range map[string]string{
		"source_id": "120210000", "source_type": "ad", "ctwa_clid": "CLID123",
		"headline": "¿Respondes mensajes todo el día?", "source_url": "https://fb.me/x",
	} {
		if got.Referral[key] != want {
			t.Errorf("referral[%q] = %v, esperaba %q", key, got.Referral[key], want)
		}
	}
}

// Contrapartida de la anterior: el cambio toca el camino caliente del webhook y
// el 100 % del tráfico de hoy llega **sin** referral. Un mensaje normal debe
// seguir dando exactamente el mismo Raw que antes.
func TestParseRawDeMensajeSinReferral(t *testing.T) {
	payload := `{"entry":[{"changes":[{"value":{"metadata":{"phone_number_id":"PNID"},"messages":[
		{"from":"51999","id":"wamid.1","type":"image","image":{"id":"MID","mime_type":"image/jpeg","sha256":"abc","caption":"pago"}}
	]}}]}]}`
	msgs, err := Parse([]byte(payload))
	if err != nil || len(msgs) != 1 {
		t.Fatalf("Parse: len=%d err=%v", len(msgs), err)
	}
	if msgs[0].MediaID != "MID" || msgs[0].Caption != "pago" || msgs[0].Type != channels.MsgImage {
		t.Fatalf("imagen mal parseada: %+v", msgs[0])
	}
	var got map[string]any
	if err := json.Unmarshal(msgs[0].Raw, &got); err != nil {
		t.Fatalf("Raw no es JSON válido: %v", err)
	}
	if got["id"] != "wamid.1" || got["type"] != "image" {
		t.Fatalf("Raw perdió campos del mensaje: %v", got)
	}
}

// Con varios mensajes en el mismo lote, cada Raw debe ser el suyo. encoding/json
// reutiliza el buffer que entrega a UnmarshalJSON, así que quedarse con el slice
// sin copiarlo dejaría a todos apuntando a los bytes del último.
func TestParseRawNoSeMezclaEntreMensajes(t *testing.T) {
	payload := `{"entry":[{"changes":[{"value":{"metadata":{"phone_number_id":"PNID"},"messages":[
		{"from":"51999","id":"wamid.1","type":"text","text":{"body":"uno"},"referral":{"source_id":"AD-1"}},
		{"from":"51999","id":"wamid.2","type":"text","text":{"body":"dos"},"referral":{"source_id":"AD-2"}}
	]}}]}]}`
	msgs, err := Parse([]byte(payload))
	if err != nil || len(msgs) != 2 {
		t.Fatalf("Parse: len=%d err=%v", len(msgs), err)
	}
	for i, wantID := range []string{"AD-1", "AD-2"} {
		var got struct {
			Referral struct {
				SourceID string `json:"source_id"`
			} `json:"referral"`
		}
		if err := json.Unmarshal(msgs[i].Raw, &got); err != nil {
			t.Fatalf("Raw[%d] no es JSON válido: %v", i, err)
		}
		if got.Referral.SourceID != wantID {
			t.Errorf("Raw[%d].referral.source_id = %q, esperaba %q", i, got.Referral.SourceID, wantID)
		}
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
