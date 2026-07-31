package models

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Auditoría de las vistas: qué registros quedan fuera por un dato mal escrito.
//
// `safe_iso_date` (§7.3) devuelve NULL en vez de reventar la consulta, y eso es
// lo correcto para no abortar una campaña entera. El efecto secundario es que
// esos registros desaparecen en silencio: son clientes a los que nunca se les
// cobraría y que hoy nadie vería. Estas funciones los sacan a la luz.

// GetDataViewForBot carga una vista comprobando que pertenece a la org del bot.
//
// Las columnas van calificadas a mano y no con `dataViewCols`: esa constante no
// lleva prefijo de tabla y `data_objects` tiene también `id`, `name`,
// `created_at` y `updated_at`, así que en un JOIN Postgres responde
// "column reference is ambiguous" en tiempo de ejecución, no al compilar.
func GetDataViewForBot(ctx context.Context, p *pgxpool.Pool, botID, viewID string) (*DataView, error) {
	rows, err := p.Query(ctx, `SELECT v.id::text AS id, v.object_id::text AS object_id,
		v.name, v.filter, v.created_at, v.updated_at
		FROM data_views v JOIN data_objects o ON o.id = v.object_id
		WHERE v.id = $1::uuid AND o.org_id = (SELECT org_id FROM bots WHERE id = $2::uuid)`,
		viewID, botID)
	if err != nil {
		return nil, err
	}
	view, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[DataView])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &view, nil
}

// InvalidDateRecord es un registro descartado por tener una fecha ilegible en
// un campo que la vista filtra por fecha.
type InvalidDateRecord struct {
	RecordID string `json:"recordId"`
	Field    string `json:"field"`
	Value    string `json:"value"`
}

// dateFieldsOfView devuelve los campos que la vista compara como fecha, ya sea
// por un operador relativo o porque el campo está tipado como `date`.
func dateFieldsOfView(view *DataView, fields []DataField) []string {
	types := map[string]string{}
	for _, field := range fields {
		types[field.Key] = field.Type
	}
	var filter DataFilter
	if len(view.Filter) > 0 && json.Unmarshal(view.Filter, &filter) != nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(filter.Where))
	for _, rule := range filter.Where {
		isDate := types[rule.Field] == "date" ||
			(strings.HasPrefix(rule.Op, "date_") && strings.HasSuffix(rule.Op, "_relative"))
		if !isDate || seen[rule.Field] {
			continue
		}
		seen[rule.Field] = true
		out = append(out, rule.Field)
	}
	return out
}

// InvalidDateRecordsForView lista los registros del objeto de la vista cuyo
// valor de fecha existe pero no es una fecha ISO válida. Un valor vacío no
// cuenta: eso es un dato que falta, no un dato roto.
func InvalidDateRecordsForView(ctx context.Context, p *pgxpool.Pool, botID, viewID string, limit int) ([]InvalidDateRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	view, err := GetDataViewForBot(ctx, p, botID, viewID)
	if err != nil || view == nil {
		return nil, err
	}
	fields, err := ListDataFields(ctx, p, botID, view.ObjectID)
	if err != nil {
		return nil, err
	}
	out := make([]InvalidDateRecord, 0)
	for _, key := range dateFieldsOfView(view, fields) {
		rows, err := p.Query(ctx, `SELECT id::text AS record_id, $2::text AS field,
			data ->> $2 AS value FROM data_records
			WHERE object_id = $1::uuid AND NULLIF(data ->> $2, '') IS NOT NULL
			  AND safe_iso_date(data ->> $2) IS NULL
			ORDER BY created_at DESC LIMIT $3`, view.ObjectID, key, limit)
		if err != nil {
			return nil, err
		}
		found, err := pgx.CollectRows(rows, pgx.RowToStructByName[InvalidDateRecord])
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
		if len(out) >= limit {
			return out[:limit], nil
		}
	}
	return out, nil
}
