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

// Lectura genérica detrás de la tool `data_query`.
//
// Es el gemelo de lectura de `MutateDataRecord` y comparte su regla dura: nada
// de lo que llega desde el mensaje del cliente puede elegir organización, tabla
// ni campo. La organización la impone el backend a partir del bot; el objeto y
// los campos los fijó el autor del flujo al publicar; solo los **valores** de
// comparación son variables. Por eso no hay ningún camino desde el texto del
// usuario hasta un nombre de columna.

const (
	// dataQueryDefaultLimit mantiene barata la lectura habitual: en el grafo casi
	// siempre se busca un registro (el perfil del contacto), no una lista.
	dataQueryDefaultLimit = 10
	// dataQueryMaxLimit es el techo absoluto. El autor puede bajarlo, nunca subirlo.
	dataQueryMaxLimit = 50
	dataQueryMaxRules = 8
)

// DataQueryRule es una condición sobre un campo del objeto. Field y Op los fija
// el autor; Value puede venir interpolado de una variable del flujo.
type DataQueryRule struct {
	Field string
	Op    string
	Value string
}

type DataQueryInput struct {
	OrgID     string
	ObjectKey string
	// Fields limita lo que se devuelve. Vacío entrega todos los campos del objeto.
	Fields []string
	Where  []DataQueryRule
	// Text busca en todo el registro, como el buscador del panel. Es lo que usa el
	// modelo cuando no conoce el nombre de un campo.
	Text string
	// LinkedContactPhone restringe a los registros vinculados a ese contacto. El
	// teléfono lo pone el runtime desde el mensaje entrante, nunca el grafo.
	LinkedContactPhone string
	OrderBy            string
	OrderDesc          bool
	Limit              int
}

// DataQueryRecord es un registro proyectado. Se expone `recordId` y no el
// `object_id` interno: el flujo referencia objetos por clave, no por UUID.
type DataQueryRecord struct {
	RecordID string         `json:"recordId"`
	Data     map[string]any `json:"data"`
}

// DataQueryResult es el contrato que ven el grafo y el modelo.
//
// Cero coincidencias es un resultado correcto, no un fallo: el Router debe poder
// decidir por `found=false` sin pasar por la rama `error`, que queda reservada a
// configuración inválida o consulta rota.
type DataQueryResult struct {
	Found   bool              `json:"found"`
	Count   int               `json:"count"`
	First   *DataQueryRecord  `json:"first"`
	Records []DataQueryRecord `json:"records"`
}

// dataQueryOps traduce el operador declarado a SQL. `contains`, `in` y `exists`
// se resuelven aparte porque no son una comparación binaria directa.
var dataQueryOps = map[string]string{
	"eq": "=", "neq": "!=", "lt": "<", "lte": "<=", "gt": ">", "gte": ">=",
}

// DataQueryOperators son los operadores admitidos, en orden estable para la
// interfaz y los mensajes de error.
var DataQueryOperators = []string{"eq", "neq", "lt", "lte", "gt", "gte", "contains", "in", "exists"}

// ValidDataQueryOperator existe para que el validador del grafo rechace un
// operador inventado antes de publicar, sin duplicar la lista.
func ValidDataQueryOperator(op string) bool {
	if _, ok := dataQueryOps[op]; ok {
		return true
	}
	return op == "contains" || op == "in" || op == "exists"
}

func QueryDataRecords(ctx context.Context, pool *pgxpool.Pool, input DataQueryInput) (*DataQueryResult, error) {
	input.ObjectKey = strings.TrimSpace(input.ObjectKey)
	if input.OrgID == "" || input.ObjectKey == "" {
		return nil, errors.New("organización y objeto son obligatorios")
	}
	if len(input.Where) > dataQueryMaxRules {
		return nil, fmt.Errorf("data_query admite hasta %d condiciones", dataQueryMaxRules)
	}
	if input.Limit <= 0 {
		input.Limit = dataQueryDefaultLimit
	}
	if input.Limit > dataQueryMaxLimit {
		input.Limit = dataQueryMaxLimit
	}

	var objectID string
	err := pool.QueryRow(ctx, `SELECT id::text FROM data_objects WHERE org_id=$1::uuid AND key=$2`,
		input.OrgID, input.ObjectKey).Scan(&objectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("el objeto %q no existe en la organización", input.ObjectKey)
	}
	if err != nil {
		return nil, err
	}

	rows, err := pool.Query(ctx, `SELECT `+dataFieldCols+` FROM data_fields
		WHERE object_id=$1::uuid ORDER BY created_at`, objectID)
	if err != nil {
		return nil, err
	}
	fields, err := pgx.CollectRows(rows, pgx.RowToStructByName[DataField])
	if err != nil {
		return nil, err
	}
	fieldTypes := make(map[string]string, len(fields))
	for _, field := range fields {
		fieldTypes[field.Key] = field.Type
	}

	projection, err := dataQueryProjection(fieldTypes, input.Fields)
	if err != nil {
		return nil, err
	}

	query := `SELECT r.id::text AS id, r.data FROM data_records r`
	args := []any{objectID}
	if phone := NormalizePhone(input.LinkedContactPhone); phone != "" {
		args = append(args, input.OrgID, phone)
		query += ` JOIN data_record_contacts rc ON rc.record_id = r.id
			JOIN contacts c ON c.id = rc.contact_id
				AND c.org_id = $` + strconv.Itoa(len(args)-1) + `::uuid
				AND c.phone_normalized = $` + strconv.Itoa(len(args))
	} else if strings.TrimSpace(input.LinkedContactPhone) != "" {
		// Un teléfono presente pero no normalizable devolvería la tabla entera si
		// se ignorara el filtro en silencio. Preferimos el resultado vacío.
		return &DataQueryResult{Records: []DataQueryRecord{}}, nil
	}
	query += ` WHERE r.object_id = $1::uuid`

	for _, rule := range input.Where {
		condition, err := dataQueryCondition(fieldTypes, rule, &args)
		if err != nil {
			return nil, err
		}
		if condition != "" {
			query += ` AND ` + condition
		}
	}
	if text := strings.TrimSpace(input.Text); text != "" {
		args = append(args, text)
		query += ` AND r.data::text ILIKE '%' || $` + strconv.Itoa(len(args)) + ` || '%'`
	}

	order, err := dataQueryOrder(fieldTypes, input.OrderBy, input.OrderDesc, &args)
	if err != nil {
		return nil, err
	}
	// El desempate final por id no es cosmético: sin él dos registros con la misma
	// fecha se alternan entre ejecuciones y `first` deja de ser reproducible.
	query += ` ORDER BY ` + order + `r.created_at DESC, r.id LIMIT ` + strconv.Itoa(input.Limit)

	resultRows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer resultRows.Close()

	result := &DataQueryResult{Records: []DataQueryRecord{}}
	for resultRows.Next() {
		var id string
		var raw json.RawMessage
		if err := resultRows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var values map[string]any
		if json.Unmarshal(raw, &values) != nil {
			values = map[string]any{}
		}
		result.Records = append(result.Records, DataQueryRecord{
			RecordID: id, Data: projectDataQueryValues(values, projection),
		})
	}
	if err := resultRows.Err(); err != nil {
		return nil, err
	}
	result.Count = len(result.Records)
	result.Found = result.Count > 0
	if result.Found {
		first := result.Records[0]
		result.First = &first
	}
	return result, nil
}

// dataQueryProjection resuelve qué campos salen. Devuelve nil cuando el autor no
// acotó nada, que significa «todos los del objeto».
func dataQueryProjection(fieldTypes map[string]string, requested []string) (map[string]struct{}, error) {
	if len(requested) == 0 {
		return nil, nil
	}
	projection := make(map[string]struct{}, len(requested))
	for _, key := range requested {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, ok := fieldTypes[key]; !ok {
			return nil, fmt.Errorf("el campo %q no existe en el objeto", key)
		}
		projection[key] = struct{}{}
	}
	if len(projection) == 0 {
		return nil, nil
	}
	return projection, nil
}

// projectDataQueryValues descarta lo que el bloque no autorizó. Se filtra al
// salir y no en SQL porque `data` es un JSONB entero: recortarlo aquí garantiza
// que ni un campo olvidado del objeto llegue al prompt de un modelo.
//
// Los campos declarados que el registro no trae salen como nulos en vez de
// omitirse. La diferencia importa fuera de aquí: el motor proyecta cada clave a
// una variable del flujo, y una variable ausente deja su `{marcador}` literal
// dentro del prompt. Declarar el campo es lo que promete que la variable existe.
func projectDataQueryValues(values map[string]any, projection map[string]struct{}) map[string]any {
	if projection == nil {
		return values
	}
	out := make(map[string]any, len(projection))
	for key := range projection {
		out[key] = values[key]
	}
	return out
}

func dataQueryCondition(fieldTypes map[string]string, rule DataQueryRule, args *[]any) (string, error) {
	field := strings.TrimSpace(rule.Field)
	op := strings.TrimSpace(rule.Op)
	typ, ok := fieldTypes[field]
	if !ok {
		return "", fmt.Errorf("el campo %q no existe en el objeto", field)
	}
	if !ValidDataQueryOperator(op) {
		return "", fmt.Errorf("operador inválido %q", op)
	}

	*args = append(*args, field)
	keyRef := "$" + strconv.Itoa(len(*args))
	expr := `r.data ->> ` + keyRef

	switch op {
	case "exists":
		return `NULLIF(` + expr + `, '') IS NOT NULL`, nil
	case "contains":
		*args = append(*args, rule.Value)
		return expr + ` ILIKE '%' || $` + strconv.Itoa(len(*args)) + ` || '%'`, nil
	case "in":
		values := splitDataQueryList(rule.Value)
		if len(values) == 0 {
			// Una lista vacía no puede coincidir con nada. Devolver la condición
			// siempre falsa es más honesto que ignorar el filtro.
			return `FALSE`, nil
		}
		*args = append(*args, values)
		return expr + ` = ANY($` + strconv.Itoa(len(*args)) + `::text[])`, nil
	}

	operator := dataQueryOps[op]
	value := strings.TrimSpace(rule.Value)
	switch typ {
	case "number":
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			return "", fmt.Errorf("el valor de %q debe ser numérico", field)
		}
		*args = append(*args, value)
		return `NULLIF(` + expr + `, '')::numeric ` + operator + ` $` + strconv.Itoa(len(*args)) + `::numeric`, nil
	case "date":
		if _, err := time.Parse("2006-01-02", value); err != nil {
			return "", fmt.Errorf("el valor de %q debe tener formato AAAA-MM-DD", field)
		}
		*args = append(*args, value)
		return `safe_iso_date(` + expr + `) ` + operator + ` $` + strconv.Itoa(len(*args)) + `::date`, nil
	case "boolean":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return "", fmt.Errorf("el valor de %q debe ser verdadero o falso", field)
		}
		// Un boolean guardado por el panel puede ser `true` o la cadena "true"; el
		// JSONB conserva la diferencia y `->>` la devuelve igual en ambos casos.
		*args = append(*args, strconv.FormatBool(parsed))
		return expr + ` ` + operator + ` $` + strconv.Itoa(len(*args)), nil
	default:
		*args = append(*args, rule.Value)
		return expr + ` ` + operator + ` $` + strconv.Itoa(len(*args)), nil
	}
}

func dataQueryOrder(fieldTypes map[string]string, orderBy string, desc bool, args *[]any) (string, error) {
	orderBy = strings.TrimSpace(orderBy)
	if orderBy == "" {
		return "", nil
	}
	direction := " ASC, "
	if desc {
		direction = " DESC, "
	}
	switch orderBy {
	case "createdAt":
		return `r.created_at` + direction, nil
	case "updatedAt":
		return `r.updated_at` + direction, nil
	}
	typ, ok := fieldTypes[orderBy]
	if !ok {
		return "", fmt.Errorf("no se puede ordenar por %q: el campo no existe en el objeto", orderBy)
	}
	*args = append(*args, orderBy)
	expr := `r.data ->> $` + strconv.Itoa(len(*args))
	switch typ {
	case "number":
		// NULLS LAST evita que los registros sin valor encabecen el orden y se
		// lleven `first`, que es justo el que decide la rama del Router.
		return `NULLIF(` + expr + `, '')::numeric` + strings.TrimSuffix(direction, ", ") + ` NULLS LAST, `, nil
	case "date":
		return `safe_iso_date(` + expr + `)` + strings.TrimSuffix(direction, ", ") + ` NULLS LAST, `, nil
	default:
		return expr + strings.TrimSuffix(direction, ", ") + ` NULLS LAST, `, nil
	}
}

func splitDataQueryList(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
