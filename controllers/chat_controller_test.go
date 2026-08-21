package controllers

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Yzx7/sacs-chatbots/models"
)

func TestEventoDeModoNoTransportaLosDatosCRM(t *testing.T) {
	meta := &models.ChatMeta{
		ID:          "chat-id",
		BotID:       "bot-id",
		Contact:     "51999999999",
		ContactName: ptr("Cliente"),
		ContactData: json.RawMessage(`{"nota":"` + strings.Repeat("x", 2000) + `"}`),
		Mode:        "manual",
	}
	raw, err := json.Marshal(chatModeView(meta))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("contactData")) || len(raw) > 900 {
		t.Fatalf("el evento liviano no debe superar NOTIFY ni incluir el CRM: %d bytes", len(raw))
	}
}

func ptr(value string) *string { return &value }
