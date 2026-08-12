package models

import (
	"context"
	"strings"
	"testing"
)

func TestRecordAIUsageRejectsUnknownPurposeBeforeWriting(t *testing.T) {
	err := RecordAIUsage(context.Background(), nil, AIUsageEventInput{
		Provider: "provider",
		Model:    "model",
		Purpose:  "chat_de_prueba",
	})
	if err == nil || !strings.Contains(err.Error(), "purpose") {
		t.Fatalf("se esperaba rechazo de purpose desconocido, got %v", err)
	}
}
