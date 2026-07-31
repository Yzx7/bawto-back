package controllers

import (
	"testing"

	"github.com/Yzx7/sacs-chatbots/channels"
	"github.com/Yzx7/sacs-chatbots/models"
)

func TestShouldBlockAmbiguousReceipt(t *testing.T) {
	ambiguous := &models.ReminderCorrelation{Method: "ambiguous", CandidateCount: 2}
	if !shouldBlockAmbiguousReceipt(ambiguous, channels.MsgImage) {
		t.Fatal("una imagen con varios registros candidatos nunca debe atribuirse")
	}
	if shouldBlockAmbiguousReceipt(ambiguous, channels.MsgText) {
		t.Fatal("un texto ambiguo puede continuar para pedir una selección segura")
	}
	if shouldBlockAmbiguousReceipt(&models.ReminderCorrelation{Method: "exact"}, channels.MsgImage) {
		t.Fatal("una respuesta citada exacta debe poder procesar la imagen")
	}
}

func TestRetryableAgentOutput(t *testing.T) {
	for _, code := range []string{
		"missing_tool_call",
		"unexpected_tool",
		"multiple_tool_calls",
		"invalid_tool_input",
		"invalid_branch",
		"empty_reply",
	} {
		if !retryableAgentOutput(code) {
			t.Fatalf("%s debería reintentarse", code)
		}
	}
	for _, code := range []string{"", "provider_error", "invalid_outputs"} {
		if retryableAgentOutput(code) {
			t.Fatalf("%s no debería reintentarse", code)
		}
	}
}
