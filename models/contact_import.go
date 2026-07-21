package models

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const maxContactImportRows = 10000

type ContactImportRow struct {
	Row    int             `json:"row"`
	Phone  string          `json:"phone"`
	Name   string          `json:"name,omitempty"`
	Status string          `json:"status,omitempty"`
	Data   json.RawMessage `json:"data"`
	Error  string          `json:"error,omitempty"`
}

type ContactImportReport struct {
	Total   int                `json:"total"`
	Valid   int                `json:"valid"`
	Invalid int                `json:"invalid"`
	Rows    []ContactImportRow `json:"rows"`
}

// ParseContactsCSV convierte un CSV con encabezados a contactos. Las columnas
// distintas de phone/name/status se conservan como atributos personalizados.
func ParseContactsCSV(r io.Reader, phoneColumn, nameColumn, statusColumn string) (ContactImportReport, error) {
	reader := csv.NewReader(io.LimitReader(r, 10<<20)) // 10 MiB
	reader.TrimLeadingSpace = true
	headers, err := reader.Read()
	if err != nil {
		return ContactImportReport{}, fmt.Errorf("leyendo encabezados: %w", err)
	}
	indexes := make(map[string]int, len(headers))
	for i, header := range headers {
		indexes[canonicalColumn(header)] = i
	}
	if phoneColumn == "" {
		phoneColumn = "phone"
	}
	if nameColumn == "" {
		nameColumn = "name"
	}
	if statusColumn == "" {
		statusColumn = "status"
	}
	phoneIndex, ok := indexes[canonicalColumn(phoneColumn)]
	if !ok {
		return ContactImportReport{}, fmt.Errorf("no existe la columna de teléfono %q", phoneColumn)
	}
	nameIndex, hasName := indexes[canonicalColumn(nameColumn)]
	statusIndex, hasStatus := indexes[canonicalColumn(statusColumn)]
	report := ContactImportReport{}
	for rowNum := 2; rowNum <= maxContactImportRows+1; rowNum++ {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return report, fmt.Errorf("fila %d: %w", rowNum, err)
		}
		report.Total++
		row := ContactImportRow{Row: rowNum, Data: json.RawMessage(`{}`)}
		if phoneIndex >= len(record) {
			row.Error = "teléfono ausente"
		} else {
			row.Phone = NormalizePhone(record[phoneIndex])
		}
		if hasName && nameIndex < len(record) {
			row.Name = strings.TrimSpace(record[nameIndex])
		}
		if hasStatus && statusIndex < len(record) {
			row.Status = strings.TrimSpace(record[statusIndex])
		}
		if row.Status == "" {
			row.Status = "active"
		}
		if row.Status != "active" && row.Status != "inactive" && row.Status != "blocked" {
			row.Error = "estado inválido"
		}
		if len(row.Phone) < 6 || len(row.Phone) > 20 {
			row.Error = "teléfono inválido"
		}
		data := map[string]string{}
		for i, value := range record {
			key := canonicalColumn(headers[i])
			if i == phoneIndex || (hasName && i == nameIndex) || (hasStatus && i == statusIndex) || key == "" {
				continue
			}
			data[key] = strings.TrimSpace(value)
		}
		row.Data, _ = json.Marshal(data)
		if row.Error == "" {
			report.Valid++
		} else {
			report.Invalid++
		}
		report.Rows = append(report.Rows, row)
	}
	if report.Total == maxContactImportRows {
		return report, fmt.Errorf("el archivo supera el máximo de %d filas", maxContactImportRows)
	}
	return report, nil
}

func canonicalColumn(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.NewReplacer(" ", "_", "-", "_", ".", "_").Replace(value)
	return value
}
