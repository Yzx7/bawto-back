package models

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type DataRecordImportRow struct {
	Row   int               `json:"row"`
	Data  map[string]any    `json:"data,omitempty"`
	Error string            `json:"error,omitempty"`
	Raw   map[string]string `json:"raw,omitempty"`
}

type DataRecordImportReport struct {
	Total   int                   `json:"total"`
	Valid   int                   `json:"valid"`
	Invalid int                   `json:"invalid"`
	Rows    []DataRecordImportRow `json:"rows"`
}

// ParseDataRecordsCSV usa las claves de data_fields como encabezados. Convierte
// números/boolean/json y pasa cada fila por la misma validación que la API.
func ParseDataRecordsCSV(reader io.Reader, fields []DataField) (*DataRecordImportReport, error) {
	csvReader := csv.NewReader(reader)
	csvReader.TrimLeadingSpace = true
	csvReader.FieldsPerRecord = -1
	headers, err := csvReader.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("el CSV está vacío")
		}
		return nil, fmt.Errorf("encabezado CSV inválido: %w", err)
	}
	byKey := map[string]DataField{}
	for _, field := range fields {
		byKey[field.Key] = field
	}
	seen := map[string]bool{}
	for i := range headers {
		headers[i] = strings.TrimSpace(strings.TrimPrefix(headers[i], "\ufeff"))
		if headers[i] == "" {
			return nil, fmt.Errorf("el encabezado de la columna %d está vacío", i+1)
		}
		if seen[headers[i]] {
			return nil, fmt.Errorf("el encabezado %q está duplicado", headers[i])
		}
		seen[headers[i]] = true
		if _, ok := byKey[headers[i]]; !ok {
			return nil, fmt.Errorf("la columna %q no corresponde a ningún campo del objeto", headers[i])
		}
	}
	if len(headers) == 0 {
		return nil, errors.New("el CSV no tiene columnas")
	}

	report := &DataRecordImportReport{}
	for rowNumber := 2; ; rowNumber++ {
		values, readErr := csvReader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		report.Total++
		row := DataRecordImportRow{Row: rowNumber, Data: map[string]any{}, Raw: map[string]string{}}
		if readErr != nil {
			row.Error = readErr.Error()
			report.Invalid++
			report.Rows = append(report.Rows, row)
			continue
		}
		if len(values) != len(headers) {
			row.Error = fmt.Sprintf("se esperaban %d columnas y llegaron %d", len(headers), len(values))
			report.Invalid++
			report.Rows = append(report.Rows, row)
			continue
		}
		for i, raw := range values {
			key := headers[i]
			raw = strings.TrimSpace(raw)
			row.Raw[key] = raw
			if raw == "" {
				continue
			}
			field := byKey[key]
			var parsed any = raw
			switch field.Type {
			case "number":
				number, parseErr := strconv.ParseFloat(raw, 64)
				if parseErr != nil {
					row.Error = fmt.Sprintf("%s debe ser numérico", field.Label)
				} else {
					parsed = number
				}
			case "boolean":
				boolean, parseErr := strconv.ParseBool(raw)
				if parseErr != nil {
					row.Error = fmt.Sprintf("%s debe ser true o false", field.Label)
				} else {
					parsed = boolean
				}
			case "json":
				var value any
				if json.Unmarshal([]byte(raw), &value) != nil {
					row.Error = fmt.Sprintf("%s debe contener JSON válido", field.Label)
				} else {
					parsed = value
				}
			}
			if row.Error != "" {
				break
			}
			row.Data[key] = parsed
		}
		if row.Error == "" {
			data, _ := json.Marshal(row.Data)
			if validationErr := ValidateDataRecord(fields, data); validationErr != nil {
				row.Error = validationErr.Error()
			}
		}
		if row.Error == "" {
			report.Valid++
			row.Raw = nil
		} else {
			report.Invalid++
			row.Data = nil
		}
		report.Rows = append(report.Rows, row)
		if report.Total >= 20000 {
			return nil, errors.New("el CSV supera el máximo de 20000 registros")
		}
	}
	return report, nil
}
