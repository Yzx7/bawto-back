package models

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Consulta sobre el propio contacto, gemela de `data_query` pero contra
// `contacts` en vez de `data_records`.
//
// Existe porque `contacts` es la única tabla del producto que NO es un
// `data_object`: es especial por diseño —representa al destinatario de los
// canales—, tiene sus columnas propias (`status`, `name`, `phone_normalized`) y
// su JSONB descrito por `contact_fields`. Y es, a la vez, la tabla más natural
// sobre la que definir una audiencia: «los contactos con `piloto = si`» no
// debería obligar a crear una tabla aparte y vincular fila por fila lo que el
// campo de contacto ya expresa.
//
// La rama que esto introduce **no es un caso de negocio**, que es lo que el
// proyecto prohíbe ramificar: es dónde vive el dato, una distinción real y
// permanente del esquema. Los nueve operadores siguen definidos una sola vez, en
// `dataQueryComparison`.

// contactBuiltinFields son las columnas propias de `contacts`, consultables por
// su nombre corto.
//
// **Tienen precedencia sobre un `contact_field` que se llame igual.** La
// alternativa —que un campo personalizado tapara la columna— dejaría al operador
// filtrando por un dato que no es el que la pantalla le muestra en esa columna.
var contactBuiltinFields = map[string]string{
	// La columna real es phone_normalized; se expone como `phone` porque es como
	// la nombra el resto de la interfaz.
	"phone":  "c.phone_normalized",
	"name":   "c.name",
	"status": "c.status",
}

// ContactQueryField describe un campo por el que se puede filtrar un contacto.
// Lo consume el panel para ofrecer el selector, y el ejecutor para tipar.
type ContactQueryField struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Type    string   `json:"type"`
	Builtin bool     `json:"builtin"`
	Options []string `json:"options,omitempty"`
}

// ContactQueryFields entrega los campos filtrables de la organización: primero
// las columnas propias, luego los campos personalizados que no las tapan.
func ContactQueryFields(ctx context.Context, p *pgxpool.Pool, orgID string) ([]ContactQueryField, error) {
	out := []ContactQueryField{
		{Key: "status", Label: "Estado", Type: "text", Builtin: true,
			Options: []string{"active", "inactive", "blocked"}},
		{Key: "name", Label: "Nombre", Type: "text", Builtin: true},
		{Key: "phone", Label: "Teléfono", Type: "text", Builtin: true},
	}
	fields, err := ListContactFieldsByOrg(ctx, p, orgID)
	if err != nil {
		return nil, err
	}
	for _, field := range fields {
		if _, taken := contactBuiltinFields[field.Key]; taken {
			continue
		}
		out = append(out, ContactQueryField{Key: field.Key, Label: field.Label, Type: field.Type})
	}
	return out, nil
}

// contactMatches decide si el contacto de este teléfono cumple las condiciones.
//
// Devuelve error si alguna condición no se puede evaluar —campo inexistente,
// valor con el tipo equivocado—. El llamador traduce eso a «no atiende», nunca a
// «no hay restricción»: ver ContactMatchesAudience.
func contactMatches(ctx context.Context, p *pgxpool.Pool, orgID, phone string, rules []DataQueryRule) (bool, error) {
	types := map[string]string{}
	fields, err := ContactQueryFields(ctx, p, orgID)
	if err != nil {
		return false, err
	}
	for _, field := range fields {
		types[field.Key] = field.Type
	}

	// El teléfono lo pone el runtime desde el mensaje entrante, nunca el grafo:
	// es lo que ata la condición a ESTE contacto y no a «alguno que cumpla».
	args := []any{orgID, NormalizePhone(phone)}
	query := `SELECT 1 FROM contacts c WHERE c.org_id = $1::uuid AND c.phone_normalized = $2`

	where, err := contactConditions(types, rules, &args)
	if err != nil {
		return false, err
	}
	query += where + ` LIMIT 1`

	var found int
	err = p.QueryRow(ctx, query, args...).Scan(&found)
	// Sin fila no es un fallo de la consulta: es que el contacto no cumple. Se
	// distingue por el error tipado de pgx y no por el texto del mensaje, que
	// cambia entre versiones y dejaría un «no cumple» convertido en avería.
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// contactConditions arma el fragmento ` AND …` de las reglas sobre un contacto
// con alias `c`.
//
// Lo comparten el predicado del despacho y la vista previa del panel **a
// propósito**: una vista previa que enseñara un conjunto distinto del que el bot
// atiende sería peor que no tener vista previa, porque daría confianza falsa
// justo antes de publicar. Compartir el constructor es lo único que garantiza
// que no se separen.
func contactConditions(types map[string]string, rules []DataQueryRule, args *[]any) (string, error) {
	var out string
	for _, rule := range rules {
		field := strings.TrimSpace(rule.Field)
		typ, known := types[field]
		if !known {
			return "", &contactFieldError{field: field}
		}
		expr, builtin := contactBuiltinFields[field]
		if !builtin {
			*args = append(*args, field)
			expr = `c.data ->> $` + strconv.Itoa(len(*args))
		}
		condition, err := dataQueryComparison(expr, field, typ, rule, args)
		if err != nil {
			return "", err
		}
		out += ` AND ` + condition
	}
	return out, nil
}

type contactFieldError struct{ field string }

func (e *contactFieldError) Error() string {
	return "el campo " + strconv.Quote(e.field) + " no existe en los contactos"
}
