package models

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yzx7/sacs-chatbots/engine"
)

// Ensayo en seco de una condición de audiencia: a quién atendería el flujo.
//
// Es el gemelo inverso de `ContactMatchesAudience`. El predicado responde «¿este
// contacto entra?»; esto responde «¿quiénes entran?». Los dos **comparten los
// constructores de condiciones** —`contactConditions` y `dataQueryCondition`—
// porque una vista previa que enseñara un conjunto distinto del que el bot
// atiende sería peor que no tener ninguna: daría confianza falsa justo en el
// momento de publicar.
//
// Mismo espíritu que el ensayo en seco del `schedule`: en esta plataforma
// equivocarse de destinatario cuesta dinero y quema ventanas de 24 h, así que
// ver la lista antes es parte del flujo de trabajo, no un adorno.

// audiencePreviewMax acota lo que viaja al panel. La cuenta total se calcula
// aparte y sin límite: enseñar «12 de 3.480» es la información que decide si la
// condición está bien, y truncar en silencio la escondería.
const audiencePreviewMax = 25

// AudienceContact es un contacto de la vista previa, con lo justo para
// reconocerlo en una tabla.
type AudienceContact struct {
	ID     string  `json:"id"`
	Phone  string  `json:"phone"`
	Name   *string `json:"name,omitempty"`
	Status string  `json:"status"`
}

// AudiencePreview es el resultado del ensayo en seco.
type AudiencePreview struct {
	// Unrestricted: la condición está vacía, así que el flujo atiende a todos.
	Unrestricted bool              `json:"unrestricted"`
	Total        int               `json:"total"`
	Contacts     []AudienceContact `json:"contacts"`
	Truncated    bool              `json:"truncated"`
}

// PreviewAudience resuelve la condición contra los contactos de la organización.
//
// Devuelve error si la condición no valida o no se puede ejecutar. El panel lo
// muestra tal cual: aquí el autor **quiere** ver el fallo, al revés que en el
// despacho, donde un error se traduce a «no atiende a nadie» y se registra.
func PreviewAudience(ctx context.Context, p *pgxpool.Pool, orgID string, raw json.RawMessage) (*AudiencePreview, error) {
	condition, err := engine.ParseAudience(raw)
	if err != nil {
		return nil, err
	}

	base, args, err := audiencePreviewQuery(ctx, p, orgID, condition)
	if err != nil {
		return nil, err
	}

	var total int
	if err := p.QueryRow(ctx, `SELECT count(DISTINCT c.id) `+base, args...).Scan(&total); err != nil {
		return nil, err
	}

	// El orden es el mismo que usa la lista de contactos del panel, para que la
	// vista previa y esa pantalla no parezcan hablar de conjuntos distintos.
	//
	// El desempate va por `c.id::text` y no por `c.id`: con DISTINCT, Postgres
	// exige que toda expresión del ORDER BY esté en la proyección, y ahí está el
	// texto, no el uuid. Con `c.id` responde 42P10 en ejecución, no al compilar.
	rows, err := p.Query(ctx,
		`SELECT DISTINCT c.id::text, c.phone_normalized, c.name, c.status, c.created_at `+base+
			` ORDER BY c.created_at DESC, c.id::text LIMIT `+strconv.Itoa(audiencePreviewMax), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	preview := &AudiencePreview{
		Unrestricted: condition == nil,
		Total:        total,
		Contacts:     []AudienceContact{},
	}
	for rows.Next() {
		var c AudienceContact
		var createdAt any
		if err := rows.Scan(&c.ID, &c.Phone, &c.Name, &c.Status, &createdAt); err != nil {
			return nil, err
		}
		preview.Contacts = append(preview.Contacts, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	preview.Truncated = total > len(preview.Contacts)
	return preview, nil
}

// audiencePreviewQuery arma el `FROM … WHERE …` común al recuento y al listado,
// para que los dos no puedan discrepar.
func audiencePreviewQuery(ctx context.Context, p *pgxpool.Pool, orgID string, condition engine.AudienceCondition) (string, []any, error) {
	args := []any{orgID}

	// Sin condición el flujo atiende a todos: la vista previa son los contactos
	// de la organización, que es exactamente lo que el bot vería.
	if condition == nil {
		return `FROM contacts c WHERE c.org_id = $1::uuid`, args, nil
	}

	rules := audienceRules(condition)

	if condition["object"] == engine.AudienceContactsObject {
		types := map[string]string{}
		fields, err := ContactQueryFields(ctx, p, orgID)
		if err != nil {
			return "", nil, err
		}
		for _, field := range fields {
			types[field.Key] = field.Type
		}
		where, err := contactConditions(types, rules, &args)
		if err != nil {
			return "", nil, err
		}
		return `FROM contacts c WHERE c.org_id = $1::uuid` + where, args, nil
	}

	// Tabla de datos: entran los contactos con un registro vinculado que cumpla.
	// El JOIN es el mismo vínculo que `linkCurrentContact` resuelve en el
	// despacho, recorrido en la otra dirección.
	objectKey := strings.TrimSpace(condition["object"])
	var objectID string
	err := p.QueryRow(ctx, `SELECT id::text FROM data_objects WHERE org_id=$1::uuid AND key=$2`,
		orgID, objectKey).Scan(&objectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, errors.New("el objeto \"" + objectKey + "\" no existe en la organización")
	}
	if err != nil {
		return "", nil, err
	}

	rows, err := p.Query(ctx, `SELECT `+dataFieldCols+` FROM data_fields
		WHERE object_id=$1::uuid ORDER BY created_at`, objectID)
	if err != nil {
		return "", nil, err
	}
	fields, err := pgx.CollectRows(rows, pgx.RowToStructByName[DataField])
	if err != nil {
		return "", nil, err
	}
	fieldTypes := make(map[string]string, len(fields))
	for _, field := range fields {
		fieldTypes[field.Key] = field.Type
	}

	args = append(args, objectID)
	query := `FROM contacts c
		JOIN data_record_contacts rc ON rc.contact_id = c.id
		JOIN data_records r ON r.id = rc.record_id
		WHERE c.org_id = $1::uuid AND r.object_id = $` + strconv.Itoa(len(args)) + `::uuid`

	// Mismo constructor que usa el ejecutor de `data_query`, con el mismo alias
	// `r`: por eso la vista previa no puede interpretar una regla de otra forma.
	for _, rule := range rules {
		condition, err := dataQueryCondition(fieldTypes, rule, &args)
		if err != nil {
			return "", nil, err
		}
		if condition != "" {
			query += ` AND ` + condition
		}
	}
	return query, args, nil
}

// audienceRules extrae las reglas numeradas en orden. Es la misma lectura que
// hace ContactMatchesAudience; vive aquí para no duplicar el recorrido.
func audienceRules(condition engine.AudienceCondition) []DataQueryRule {
	var rules []DataQueryRule
	for i := 1; i <= 8; i++ {
		prefix := "where." + strconv.Itoa(i) + "."
		field := strings.TrimSpace(condition[prefix+"field"])
		if field == "" {
			continue
		}
		rules = append(rules, DataQueryRule{
			Field: field,
			Op:    strings.TrimSpace(condition[prefix+"op"]),
			Value: condition[prefix+"value"],
		})
	}
	return rules
}
