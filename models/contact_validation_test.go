package models

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateContactData(t *testing.T) {
	fields := []ContactField{
		{Key: "plan", Label: "Plan", Type: "text", Required: true},
		{Key: "cutoff_date", Label: "Fecha de corte", Type: "date"},
		{Key: "active_service", Label: "Servicio activo", Type: "boolean"},
	}
	if err := ValidateContactData(fields, json.RawMessage(`{"plan":"Fibra","cutoff_date":"2026-08-10","active_service":"true"}`)); err != nil {
		t.Fatalf("valid data: %v", err)
	}
	if err := ValidateContactData(fields, json.RawMessage(`{"cutoff_date":"10/08/2026"}`)); err == nil || !strings.Contains(err.Error(), "Plan") {
		t.Fatalf("expected required error, got %v", err)
	}
	if err := ValidateContactData(fields, json.RawMessage(`{"plan":"Fibra","active_service":"yes"}`)); err == nil {
		t.Fatal("expected boolean error")
	}
}
