package models

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type flowCopilotResourceQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// FlowCopilotConnectionSafe es una proyección allowlist para autoría. No tiene
// campos donde accidentalmente puedan viajar credenciales, base URL, máscaras o
// el texto del último error del proveedor.
type FlowCopilotConnectionSafe struct {
	Key      string     `db:"key" json:"key"`
	Label    string     `db:"label" json:"label"`
	Driver   string     `db:"driver" json:"driver"`
	Status   string     `db:"status" json:"status"`
	LastOKAt *time.Time `db:"last_ok_at" json:"lastOKAt,omitempty"`
}

type FlowCopilotDataFieldSafe struct {
	Key      string `json:"key"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

type FlowCopilotDataObjectSafe struct {
	Key    string                     `json:"key"`
	Fields []FlowCopilotDataFieldSafe `json:"fields"`
}

type FlowCopilotTemplateSafe struct {
	Name     string `db:"name" json:"name"`
	Language string `db:"language" json:"language"`
	Status   string `db:"status" json:"status"`
}

type FlowCopilotVariableSafe struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

type FlowCopilotResourceSnapshotSafe struct {
	DataObjects []FlowCopilotDataObjectSafe `json:"dataObjects"`
	Connections []FlowCopilotConnectionSafe `json:"connections"`
	Templates   []FlowCopilotTemplateSafe   `json:"templates"`
	Variables   []FlowCopilotVariableSafe   `json:"variables"`
}

// ListFlowCopilotConnectionsSafe no reutiliza ListExternalConnections: aquella
// consulta necesita la credencial cifrada y la URL para administración. El
// Copilot solo requiere identidad, driver y salud para validar bindings.
func ListFlowCopilotConnectionsSafe(
	ctx context.Context,
	pool *pgxpool.Pool,
	organizationID string,
) ([]FlowCopilotConnectionSafe, error) {
	return listFlowCopilotConnectionsSafeWithQuery(ctx, pool, organizationID)
}

func listFlowCopilotConnectionsSafeWithQuery(
	ctx context.Context,
	q flowCopilotResourceQueryer,
	organizationID string,
) ([]FlowCopilotConnectionSafe, error) {
	rows, err := q.Query(ctx, `
		SELECT key, label, driver, status, last_ok_at
		  FROM external_connections
		 WHERE org_id=$1::uuid
		 ORDER BY key`, organizationID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[FlowCopilotConnectionSafe])
}

// LoadFlowCopilotResourcesSafe arma exclusivamente metadata allowlist. Acepta
// pgx.Tx para que Apply valide bindings contra el mismo instante transaccional
// en el que cambia el draft.
func LoadFlowCopilotResourcesSafe(
	ctx context.Context,
	q flowCopilotResourceQueryer,
	organizationID, botID string,
) (*FlowCopilotResourceSnapshotSafe, error) {
	type objectFieldRow struct {
		ObjectKey string  `db:"object_key"`
		FieldKey  *string `db:"field_key"`
		FieldType *string `db:"field_type"`
		Required  *bool   `db:"required"`
	}
	rows, err := q.Query(ctx, `
		SELECT o.key AS object_key, f.key AS field_key, f.type AS field_type, f.required
		  FROM data_objects o
		  LEFT JOIN data_fields f ON f.object_id=o.id
		 WHERE o.org_id=$1::uuid
		 ORDER BY o.key, f.key`, organizationID)
	if err != nil {
		return nil, err
	}
	fields, err := pgx.CollectRows(rows, pgx.RowToStructByName[objectFieldRow])
	if err != nil {
		return nil, err
	}
	objects := make([]FlowCopilotDataObjectSafe, 0)
	objectIndexes := make(map[string]int)
	for _, row := range fields {
		index, ok := objectIndexes[row.ObjectKey]
		if !ok {
			index = len(objects)
			objectIndexes[row.ObjectKey] = index
			objects = append(objects, FlowCopilotDataObjectSafe{Key: row.ObjectKey, Fields: []FlowCopilotDataFieldSafe{}})
		}
		if row.FieldKey != nil && row.FieldType != nil {
			required := row.Required != nil && *row.Required
			objects[index].Fields = append(objects[index].Fields, FlowCopilotDataFieldSafe{
				Key: *row.FieldKey, Type: *row.FieldType, Required: required,
			})
		}
	}

	connections, err := listFlowCopilotConnectionsSafeWithQuery(ctx, q, organizationID)
	if err != nil {
		return nil, err
	}
	templateRows, err := q.Query(ctx, `
		SELECT t.name, t.language, t.status
		  FROM channel_templates t
		  JOIN bots b ON b.waba_id=t.waba_id
		 WHERE b.id=$1::uuid
		 ORDER BY t.name, t.language`, botID)
	if err != nil {
		return nil, err
	}
	templates, err := pgx.CollectRows(templateRows, pgx.RowToStructByName[FlowCopilotTemplateSafe])
	if err != nil {
		return nil, err
	}

	variables := make([]FlowCopilotVariableSafe, 0, len(fixedVariables)+len(fields))
	for _, variable := range fixedVariables {
		variables = append(variables, FlowCopilotVariableSafe{Name: variable.Name, Type: variable.Type})
	}
	contactRows, err := q.Query(ctx, `
		SELECT key, type FROM contact_fields WHERE org_id=$1::uuid ORDER BY key`, organizationID)
	if err != nil {
		return nil, err
	}
	type contactFieldRow struct {
		Key  string `db:"key"`
		Type string `db:"type"`
	}
	contactFields, err := pgx.CollectRows(contactRows, pgx.RowToStructByName[contactFieldRow])
	if err != nil {
		return nil, err
	}
	for _, field := range contactFields {
		variables = append(variables, FlowCopilotVariableSafe{Name: "contact_" + field.Key, Type: field.Type})
	}
	for _, row := range fields {
		if row.FieldKey != nil && row.FieldType != nil {
			variables = append(variables, FlowCopilotVariableSafe{
				Name: "data_" + row.ObjectKey + "_" + *row.FieldKey, Type: *row.FieldType,
			})
		}
	}
	return &FlowCopilotResourceSnapshotSafe{
		DataObjects: objects, Connections: connections, Templates: templates, Variables: variables,
	}, nil
}
