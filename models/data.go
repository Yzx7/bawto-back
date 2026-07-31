package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DataObject struct {
	ID         string    `db:"id" json:"id"`
	OrgID      string    `db:"org_id" json:"orgId"`
	Key        string    `db:"key" json:"key"`
	Name       string    `db:"name" json:"name"`
	PluralName string    `db:"plural_name" json:"pluralName"`
	CreatedAt  time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt  time.Time `db:"updated_at" json:"updatedAt"`
}
type DataField struct {
	ID        string    `db:"id" json:"id"`
	ObjectID  string    `db:"object_id" json:"objectId"`
	Key       string    `db:"key" json:"key"`
	Label     string    `db:"label" json:"label"`
	Type      string    `db:"type" json:"type"`
	Required  bool      `db:"required" json:"required"`
	CreatedAt time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt time.Time `db:"updated_at" json:"updatedAt"`
}
type DataRecord struct {
	ID        string          `db:"id" json:"id"`
	ObjectID  string          `db:"object_id" json:"objectId"`
	Data      json.RawMessage `db:"data" json:"data"`
	CreatedAt time.Time       `db:"created_at" json:"createdAt"`
	UpdatedAt time.Time       `db:"updated_at" json:"updatedAt"`
}
type DataView struct {
	ID        string          `db:"id" json:"id"`
	ObjectID  string          `db:"object_id" json:"objectId"`
	Name      string          `db:"name" json:"name"`
	Filter    json.RawMessage `db:"filter" json:"filter"`
	CreatedAt time.Time       `db:"created_at" json:"createdAt"`
	UpdatedAt time.Time       `db:"updated_at" json:"updatedAt"`
}
type DataFilter struct {
	Where []DataFilterRule `json:"where"`
}
type DataFilterRule struct {
	Field    string `json:"field"`
	Op       string `json:"op"` // eq | neq | lt | lte | gt | gte | exists | date_*_relative
	Value    string `json:"value,omitempty"`
	FromDays *int   `json:"fromDays,omitempty"`
	ToDays   *int   `json:"toDays,omitempty"`
}

const dataObjectCols = `id::text AS id, org_id::text AS org_id, key, name, plural_name, created_at, updated_at`
const dataFieldCols = `id::text AS id, object_id::text AS object_id, key, label, type, required, created_at, updated_at`
const dataRecordCols = `id::text AS id, object_id::text AS object_id, data, created_at, updated_at`
const dataViewCols = `id::text AS id, object_id::text AS object_id, name, filter, created_at, updated_at`

func ListDataObjects(ctx context.Context, p *pgxpool.Pool, botID string) ([]DataObject, error) {
	rows, e := p.Query(ctx, `SELECT `+dataObjectCols+` FROM data_objects WHERE org_id=(SELECT org_id FROM bots WHERE id=$1::uuid) ORDER BY created_at`, botID)
	if e != nil {
		return nil, e
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[DataObject])
}
func CreateDataObject(ctx context.Context, p *pgxpool.Pool, botID, key, name, plural string) (*DataObject, error) {
	rows, e := p.Query(ctx, `INSERT INTO data_objects(org_id,key,name,plural_name) SELECT org_id,$2,$3,$4 FROM bots WHERE id=$1::uuid RETURNING `+dataObjectCols, botID, key, name, plural)
	if e != nil {
		return nil, e
	}
	v, e := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[DataObject])
	return &v, e
}
func ListDataFields(ctx context.Context, p *pgxpool.Pool, botID, objectID string) ([]DataField, error) {
	rows, e := p.Query(ctx, `SELECT f.id::text AS id,f.object_id::text AS object_id,f.key,f.label,f.type,f.required,f.created_at,f.updated_at FROM data_fields f JOIN data_objects o ON o.id=f.object_id WHERE o.org_id=(SELECT org_id FROM bots WHERE id=$1::uuid) AND f.object_id=$2::uuid ORDER BY f.created_at`, botID, objectID)
	if e != nil {
		return nil, e
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[DataField])
}
func UpsertDataField(ctx context.Context, p *pgxpool.Pool, botID, objectID, key, label, typ string, required bool) (*DataField, error) {
	if err := ValidateDataFieldDefinition(key, label, typ); err != nil {
		return nil, err
	}
	rows, e := p.Query(ctx, `INSERT INTO data_fields(object_id,key,label,type,required) SELECT o.id,$3,$4,$5,$6 FROM data_objects o WHERE o.id=$2::uuid AND o.org_id=(SELECT org_id FROM bots WHERE id=$1::uuid) ON CONFLICT(object_id,key) DO UPDATE SET label=EXCLUDED.label,type=EXCLUDED.type,required=EXCLUDED.required RETURNING `+dataFieldCols, botID, objectID, key, label, typ, required)
	if e != nil {
		return nil, e
	}
	v, e := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[DataField])
	return &v, e
}

func ValidateDataFieldDefinition(key, label, typ string) error {
	key, label, typ = strings.TrimSpace(key), strings.TrimSpace(label), strings.TrimSpace(typ)
	if key == "" || label == "" {
		return errors.New("clave y etiqueta son obligatorias")
	}
	switch typ {
	case "text", "number", "date", "boolean", "json":
		return nil
	default:
		return fmt.Errorf("tipo de campo inválido %q", typ)
	}
}

func ValidateDataRecord(fields []DataField, data json.RawMessage) error {
	var values map[string]any
	if !json.Valid(data) || json.Unmarshal(data, &values) != nil {
		return errors.New("data debe ser un objeto JSON")
	}
	for _, f := range fields {
		v, ok := values[f.Key]
		if !ok || v == nil || strings.TrimSpace(fmt.Sprint(v)) == "" {
			if f.Required {
				return fmt.Errorf("el campo %q es obligatorio", f.Label)
			}
			continue
		}
		s := fmt.Sprint(v)
		switch f.Type {
		case "number":
			if _, e := strconv.ParseFloat(s, 64); e != nil {
				return fmt.Errorf("el campo %q debe ser numérico", f.Label)
			}
		case "date":
			if _, e := time.Parse("2006-01-02", s); e != nil {
				return fmt.Errorf("el campo %q debe tener formato AAAA-MM-DD", f.Label)
			}
		case "boolean":
			if _, e := strconv.ParseBool(s); e != nil {
				return fmt.Errorf("el campo %q debe ser verdadero o falso", f.Label)
			}
		}
	}
	return nil
}
func ListDataRecords(ctx context.Context, p *pgxpool.Pool, botID, objectID string) ([]DataRecord, error) {
	rows, e := p.Query(ctx, `SELECT r.id::text AS id,r.object_id::text AS object_id,r.data,r.created_at,r.updated_at FROM data_records r JOIN data_objects o ON o.id=r.object_id WHERE o.org_id=(SELECT org_id FROM bots WHERE id=$1::uuid) AND r.object_id=$2::uuid ORDER BY r.created_at DESC`, botID, objectID)
	if e != nil {
		return nil, e
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[DataRecord])
}
func CreateDataRecord(ctx context.Context, p *pgxpool.Pool, botID, objectID string, data json.RawMessage) (*DataRecord, error) {
	fields, e := ListDataFields(ctx, p, botID, objectID)
	if e != nil {
		return nil, e
	}
	if e = ValidateDataRecord(fields, data); e != nil {
		return nil, e
	}
	rows, e := p.Query(ctx, `INSERT INTO data_records(object_id,data) SELECT o.id,$3::jsonb FROM data_objects o WHERE o.id=$2::uuid AND o.org_id=(SELECT org_id FROM bots WHERE id=$1::uuid) RETURNING `+dataRecordCols, botID, objectID, data)
	if e != nil {
		return nil, e
	}
	v, e := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[DataRecord])
	return &v, e
}
func LinkRecordContact(ctx context.Context, p *pgxpool.Pool, botID, recordID, contactID, role string) error {
	// El `DO UPDATE` es lo que hace idempotente al vínculo. Con `DO NOTHING`, un
	// segundo intento sobre el mismo par no afecta filas y la comprobación de
	// abajo lo denunciaba como "no pertenece al bot": el error de propiedad y el
	// de repetición son indistinguibles desde `RowsAffected`.
	cmd, e := p.Exec(ctx, `INSERT INTO data_record_contacts(record_id,contact_id,role) SELECT r.id,c.id,$4 FROM data_records r JOIN data_objects o ON o.id=r.object_id JOIN contacts c ON c.org_id=o.org_id WHERE r.id=$2::uuid AND c.id=$3::uuid AND o.org_id=(SELECT org_id FROM bots WHERE id=$1::uuid) ON CONFLICT (record_id,contact_id,role) DO UPDATE SET role=EXCLUDED.role`, botID, recordID, contactID, role)
	if e != nil {
		return e
	}
	if cmd.RowsAffected() == 0 {
		return errors.New("registro o contacto no pertenece al bot")
	}
	return nil
}
func ListDataViews(ctx context.Context, p *pgxpool.Pool, botID, objectID string) ([]DataView, error) {
	rows, e := p.Query(ctx, `SELECT v.id::text AS id,v.object_id::text AS object_id,v.name,v.filter,v.created_at,v.updated_at FROM data_views v JOIN data_objects o ON o.id=v.object_id WHERE o.org_id=(SELECT org_id FROM bots WHERE id=$1::uuid) AND v.object_id=$2::uuid ORDER BY v.created_at`, botID, objectID)
	if e != nil {
		return nil, e
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[DataView])
}
func CreateDataView(ctx context.Context, p *pgxpool.Pool, botID, objectID, name string, filter json.RawMessage) (*DataView, error) {
	if len(filter) == 0 {
		filter = json.RawMessage(`{}`)
	}
	fields, err := ListDataFields(ctx, p, botID, objectID)
	if err != nil {
		return nil, err
	}
	if err = ValidateDataFilter(fields, filter); err != nil {
		return nil, err
	}
	rows, e := p.Query(ctx, `INSERT INTO data_views(object_id,name,filter) SELECT o.id,$3,$4::jsonb FROM data_objects o WHERE o.id=$2::uuid AND o.org_id=(SELECT org_id FROM bots WHERE id=$1::uuid) RETURNING `+dataViewCols, botID, objectID, name, filter)
	if e != nil {
		return nil, e
	}
	v, e := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[DataView])
	return &v, e
}

func intValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func ValidateDataFilter(fields []DataField, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "{}" {
		return nil
	}
	var filter DataFilter
	if !json.Valid(raw) || json.Unmarshal(raw, &filter) != nil {
		return errors.New("filtro de vista inválido")
	}
	fieldTypes := map[string]string{}
	for _, field := range fields {
		fieldTypes[field.Key] = field.Type
	}
	for _, rule := range filter.Where {
		typ, ok := fieldTypes[rule.Field]
		if !ok {
			return fmt.Errorf("el campo %q no existe en el objeto", rule.Field)
		}
		switch rule.Op {
		case "exists":
		case "eq", "neq", "lt", "lte", "gt", "gte":
			if typ == "date" {
				if _, err := time.Parse("2006-01-02", rule.Value); err != nil {
					return fmt.Errorf("el valor de %q debe tener formato AAAA-MM-DD", rule.Field)
				}
			}
			if typ == "number" {
				if _, err := strconv.ParseFloat(rule.Value, 64); err != nil {
					return fmt.Errorf("el valor de %q debe ser numérico", rule.Field)
				}
			}
		case "date_eq_relative", "date_before_relative", "date_after_relative", "date_between_relative":
			if typ != "date" {
				return fmt.Errorf("el operador %q solo admite campos date", rule.Op)
			}
			from, to := intValue(rule.FromDays), intValue(rule.ToDays)
			if from < -3650 || from > 3650 || to < -3650 || to > 3650 {
				return fmt.Errorf("el rango relativo de %q debe estar entre -3650 y 3650 días", rule.Field)
			}
			if rule.Op == "date_between_relative" && from > to {
				return fmt.Errorf("fromDays no puede ser mayor que toDays en %q", rule.Field)
			}
		default:
			return fmt.Errorf("operador inválido %q", rule.Op)
		}
	}
	return nil
}

// ResolveDataView ejecuta filtros declarativos contra los registros de una
// vista. No acepta SQL ni nombres de columnas libres: cada field se comprueba
// contra el esquema del objeto antes de construir la consulta.
func ResolveDataView(ctx context.Context, p *pgxpool.Pool, botID, viewID string) ([]DataRecord, error) {
	return ResolveDataViewAt(ctx, p, botID, viewID, time.Now().UTC())
}

// ResolveDataViewAt fija una sola fecha de referencia para toda la consulta.
// El caller debe pasarla ya convertida a la timezone del flujo.
func ResolveDataViewAt(
	ctx context.Context,
	p *pgxpool.Pool,
	botID, viewID string,
	reference time.Time,
) ([]DataRecord, error) {
	rows, err := p.Query(ctx, `SELECT v.id::text AS id, v.object_id::text AS object_id, v.name, v.filter, v.created_at, v.updated_at
        FROM data_views v JOIN data_objects o ON o.id = v.object_id WHERE v.id = $1::uuid AND o.org_id = (SELECT org_id FROM bots WHERE id=$2::uuid)`, viewID, botID)
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
	var filter DataFilter
	if len(view.Filter) > 0 && string(view.Filter) != "{}" && json.Unmarshal(view.Filter, &filter) != nil {
		return nil, errors.New("filtro de vista inválido")
	}
	fields, err := ListDataFields(ctx, p, botID, view.ObjectID)
	if err != nil {
		return nil, err
	}
	fieldTypes := map[string]string{}
	for _, field := range fields {
		fieldTypes[field.Key] = field.Type
	}
	if err := ValidateDataFilter(fields, view.Filter); err != nil {
		return nil, err
	}
	query := `SELECT ` + dataRecordCols + ` FROM data_records WHERE object_id = $1::uuid`
	args := []any{view.ObjectID}
	for _, rule := range filter.Where {
		typ, ok := fieldTypes[rule.Field]
		if !ok {
			return nil, fmt.Errorf("el campo %q no existe en el objeto", rule.Field)
		}
		if rule.Op == "exists" {
			args = append(args, rule.Field)
			query += ` AND NULLIF(data ->> $` + strconv.Itoa(len(args)) + `, '') IS NOT NULL`
			continue
		}
		if strings.HasPrefix(rule.Op, "date_") && strings.HasSuffix(rule.Op, "_relative") {
			args = append(args, rule.Field)
			keyPos := len(args)
			expr := `safe_iso_date(data ->> $` + strconv.Itoa(keyPos) + `)`
			today := time.Date(reference.Year(), reference.Month(), reference.Day(), 0, 0, 0, 0, reference.Location())
			switch rule.Op {
			case "date_eq_relative":
				args = append(args, today.AddDate(0, 0, intValue(rule.FromDays)))
				query += ` AND ` + expr + ` = $` + strconv.Itoa(len(args)) + `::date`
			case "date_before_relative":
				args = append(args, today.AddDate(0, 0, intValue(rule.FromDays)))
				query += ` AND ` + expr + ` < $` + strconv.Itoa(len(args)) + `::date`
			case "date_after_relative":
				args = append(args, today.AddDate(0, 0, intValue(rule.FromDays)))
				query += ` AND ` + expr + ` > $` + strconv.Itoa(len(args)) + `::date`
			case "date_between_relative":
				args = append(args, today.AddDate(0, 0, intValue(rule.FromDays)),
					today.AddDate(0, 0, intValue(rule.ToDays)))
				query += ` AND ` + expr + ` BETWEEN $` + strconv.Itoa(len(args)-1) +
					`::date AND $` + strconv.Itoa(len(args)) + `::date`
			}
			continue
		}
		operator := map[string]string{"eq": "=", "neq": "!=", "lt": "<", "lte": "<=", "gt": ">", "gte": ">="}[rule.Op]
		args = append(args, rule.Field, rule.Value)
		keyPos, valuePos := len(args)-1, len(args)
		expr := `data ->> $` + strconv.Itoa(keyPos)
		valueRef := "$" + strconv.Itoa(valuePos)
		if typ == "number" {
			expr = `NULLIF(` + expr + `, '')::numeric`
			query += ` AND ` + expr + ` ` + operator + ` ` + valueRef + `::numeric`
		} else if typ == "date" {
			expr = `safe_iso_date(` + expr + `)`
			query += ` AND ` + expr + ` ` + operator + ` ` + valueRef + `::date`
		} else {
			query += ` AND ` + expr + ` ` + operator + ` ` + valueRef
		}
	}
	query += ` ORDER BY created_at DESC LIMIT 1000`
	rows, err = p.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[DataRecord])
}

// DataRecordContext transforma el registro que inició el flujo en variables
// planas: record_id, record_object y record_<campo>.
func DataRecordContext(ctx context.Context, p *pgxpool.Pool, botID, recordID string) (map[string]string, error) {
	var record DataRecord
	var objectKey string
	err := p.QueryRow(ctx, `SELECT r.id::text, r.object_id::text, o.key, r.data, r.created_at, r.updated_at
		FROM data_records r JOIN data_objects o ON o.id = r.object_id
		WHERE r.id = $1::uuid AND o.org_id = (SELECT org_id FROM bots WHERE id=$2::uuid)`,
		recordID, botID).Scan(&record.ID, &record.ObjectID, &objectKey, &record.Data, &record.CreatedAt, &record.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	vars := map[string]string{
		"record_id": record.ID, "record_object_id": record.ObjectID, "record_object": objectKey,
	}
	var data map[string]any
	if json.Unmarshal(record.Data, &data) == nil {
		for key, value := range data {
			vars["record_"+key] = fmt.Sprint(value)
			vars["data_"+objectKey+"_"+key] = fmt.Sprint(value)
		}
	}
	return vars, nil
}

// PrimaryContactForRecord encuentra el destinatario del registro, sin conocer
// su dominio (factura, pedido, cita, etc.).
func PrimaryContactForRecord(ctx context.Context, p *pgxpool.Pool, botID, recordID string) (*Contact, error) {
	rows, err := p.Query(ctx, `SELECT `+contactColsC+` FROM contacts c
        JOIN data_record_contacts rc ON rc.contact_id = c.id
        JOIN data_records r ON r.id = rc.record_id
        JOIN data_objects o ON o.id = r.object_id
        WHERE o.org_id = (SELECT org_id FROM bots WHERE id=$1::uuid) AND r.id = $2::uuid ORDER BY CASE WHEN rc.role = 'primary' THEN 0 ELSE 1 END LIMIT 1`, botID, recordID)
	if err != nil {
		return nil, err
	}
	v, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Contact])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}
