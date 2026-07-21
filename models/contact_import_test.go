package models

import (
	"strings"
	"testing"
)

func TestParseContactsCSVMapsCustomFieldsAndNormalizesPhone(t *testing.T) {
	report, err := ParseContactsCSV(strings.NewReader("Nombre,WhatsApp,Plan,Zona\nAna,+51 999-111-222,Fibra 100,Norte\n"), "whatsapp", "nombre", "")
	if err != nil {
		t.Fatalf("ParseContactsCSV: %v", err)
	}
	if report.Valid != 1 || report.Invalid != 0 {
		t.Fatalf("unexpected report: %+v", report)
	}
	row := report.Rows[0]
	if row.Phone != "51999111222" || row.Name != "Ana" {
		t.Fatalf("unexpected row: %+v", row)
	}
	if !strings.Contains(string(row.Data), `"plan":"Fibra 100"`) {
		t.Fatalf("custom fields missing: %s", row.Data)
	}
}

func TestParseContactsCSVReportsInvalidPhone(t *testing.T) {
	report, err := ParseContactsCSV(strings.NewReader("phone,name\n12,Ana\n"), "", "", "")
	if err != nil {
		t.Fatalf("ParseContactsCSV: %v", err)
	}
	if report.Invalid != 1 || report.Rows[0].Error != "teléfono inválido" {
		t.Fatalf("unexpected report: %+v", report)
	}
}
