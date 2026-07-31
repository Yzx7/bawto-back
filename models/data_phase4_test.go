package models

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateDataFilterRelative(t *testing.T) {
	fields := []DataField{{Key: "vence", Label: "Vence", Type: "date"}}
	from, to := 0, 3
	valid, _ := json.Marshal(DataFilter{Where: []DataFilterRule{{
		Field: "vence", Op: "date_between_relative", FromDays: &from, ToDays: &to,
	}}})
	if err := ValidateDataFilter(fields, valid); err != nil {
		t.Fatalf("filtro relativo válido rechazado: %v", err)
	}

	wrong, _ := json.Marshal(DataFilter{Where: []DataFilterRule{{
		Field: "vence", Op: "eq", Value: "31/07/2026",
	}}})
	if err := ValidateDataFilter(fields, wrong); err == nil {
		t.Fatal("una fecha no ISO no debe guardarse en la vista")
	}

	from, to = 4, 2
	reversed, _ := json.Marshal(DataFilter{Where: []DataFilterRule{{
		Field: "vence", Op: "date_between_relative", FromDays: &from, ToDays: &to,
	}}})
	if err := ValidateDataFilter(fields, reversed); err == nil {
		t.Fatal("un rango relativo invertido debe rechazarse")
	}
}

func TestParseDataRecordsCSVTipado(t *testing.T) {
	fields := []DataField{
		{Key: "numero", Label: "Número", Type: "text", Required: true},
		{Key: "importe", Label: "Importe", Type: "number", Required: true},
		{Key: "vence", Label: "Vence", Type: "date", Required: true},
	}
	report, err := ParseDataRecordsCSV(strings.NewReader(
		"numero,importe,vence\nF-1,89.90,2026-07-31\nF-2,otro,31/07/2026\n",
	), fields)
	if err != nil {
		t.Fatalf("ParseDataRecordsCSV: %v", err)
	}
	if report.Total != 2 || report.Valid != 1 || report.Invalid != 1 {
		t.Fatalf("reporte inesperado: %+v", report)
	}
	if report.Rows[1].Error == "" {
		t.Fatal("la fila inválida debe explicar el error")
	}
}
