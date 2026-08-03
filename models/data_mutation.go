package models

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DataMutationInput es la primitiva segura detrás de la tool data_mutate. No
// acepta SQL, nombres de organización ni object_id arbitrarios: el objeto se
// resuelve por clave dentro de OrgID y todos los campos se cotejan con su schema.
type DataMutationInput struct {
	OrgID               string
	ObjectKey           string
	Operation           string // create | update | upsert
	RecordID            string
	MatchField          string
	MatchValue          string
	Values              map[string]any
	CurrentContactPhone string
	LinkCurrentContact  bool
	IdempotencyKey      string
}

type DataMutationResult struct {
	RecordID   string          `json:"recordId"`
	ObjectKey  string          `json:"objectKey"`
	Operation  string          `json:"operation"`
	Created    bool            `json:"created"`
	Idempotent bool            `json:"idempotent"`
	Data       json.RawMessage `json:"data"`
}

func MutateDataRecord(ctx context.Context, pool *pgxpool.Pool, input DataMutationInput) (*DataMutationResult, error) {
	input.ObjectKey = strings.TrimSpace(input.ObjectKey)
	input.Operation = strings.TrimSpace(input.Operation)
	input.RecordID = strings.TrimSpace(input.RecordID)
	input.MatchField = strings.TrimSpace(input.MatchField)
	input.MatchValue = strings.TrimSpace(input.MatchValue)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if input.OrgID == "" || input.ObjectKey == "" {
		return nil, fmt.Errorf("organización y objeto son obligatorios")
	}
	if input.Operation != "create" && input.Operation != "update" && input.Operation != "upsert" {
		return nil, fmt.Errorf("operación %q inválida", input.Operation)
	}
	if input.IdempotencyKey == "" || len(input.IdempotencyKey) > 200 {
		return nil, fmt.Errorf("idempotencyKey es obligatoria y admite hasta 200 caracteres")
	}
	if len(input.Values) == 0 || len(input.Values) > 64 {
		return nil, fmt.Errorf("values requiere entre 1 y 64 campos")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var objectID string
	if err = tx.QueryRow(ctx, `SELECT id::text FROM data_objects WHERE org_id=$1::uuid AND key=$2`,
		input.OrgID, input.ObjectKey).Scan(&objectID); err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("el objeto %q no existe en la organización", input.ObjectKey)
		}
		return nil, err
	}

	// Serializa mutaciones con la misma clave antes de leer el ledger. Así dos
	// webhooks concurrentes no crean dos filas y chocan recién al final.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		input.OrgID+":"+objectID+":"+input.IdempotencyKey); err != nil {
		return nil, err
	}
	if previous, found, findErr := previousDataMutation(ctx, tx, input.OrgID, objectID, input.ObjectKey, input.IdempotencyKey); findErr != nil {
		return nil, findErr
	} else if found {
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		return previous, nil
	}

	rows, err := tx.Query(ctx, `SELECT f.id::text AS id,f.object_id::text AS object_id,
		f.key,f.label,f.type,f.required,f.created_at,f.updated_at
		FROM data_fields f WHERE f.object_id=$1::uuid ORDER BY f.created_at`, objectID)
	if err != nil {
		return nil, err
	}
	fields, err := pgx.CollectRows(rows, pgx.RowToStructByName[DataField])
	if err != nil {
		return nil, err
	}
	if err = validateMutationFieldNames(fields, input.Values); err != nil {
		return nil, err
	}

	var recordID string
	var current map[string]any
	created := false
	switch input.Operation {
	case "create":
		created = true
	case "update":
		if input.RecordID == "" {
			return nil, fmt.Errorf("update requiere recordId")
		}
		recordID, current, err = mutationRecordByID(ctx, tx, input.OrgID, objectID, input.RecordID)
	case "upsert":
		if input.MatchField == "" || input.MatchValue == "" {
			return nil, fmt.Errorf("upsert requiere matchField y matchValue")
		}
		if !fieldExists(fields, input.MatchField) {
			return nil, fmt.Errorf("el campo de coincidencia %q no existe", input.MatchField)
		}
		recordID, current, err = mutationRecordByMatch(ctx, tx, objectID, input.MatchField, input.MatchValue)
		created = err == pgx.ErrNoRows
		if created {
			err = nil
		}
	}
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("el registro no existe o no pertenece al objeto")
		}
		return nil, err
	}

	merged := current
	if merged == nil {
		merged = make(map[string]any, len(input.Values)+1)
	}
	for key, value := range input.Values {
		merged[key] = value
	}
	if input.Operation == "upsert" {
		if existing, ok := merged[input.MatchField]; ok && strings.TrimSpace(fmt.Sprint(existing)) != input.MatchValue {
			return nil, fmt.Errorf("values.%s contradice matchValue", input.MatchField)
		}
		merged[input.MatchField] = input.MatchValue
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("values inválido: %w", err)
	}
	if err = ValidateDataRecord(fields, raw); err != nil {
		return nil, err
	}

	if created {
		err = tx.QueryRow(ctx, `INSERT INTO data_records(object_id,data) VALUES($1::uuid,$2::jsonb)
			RETURNING id::text`, objectID, raw).Scan(&recordID)
	} else {
		var updatedRaw json.RawMessage
		err = tx.QueryRow(ctx, `UPDATE data_records SET data=$3::jsonb,updated_at=NOW()
			WHERE id=$1::uuid AND object_id=$2::uuid RETURNING data`, recordID, objectID, raw).Scan(&updatedRaw)
	}
	if err != nil {
		return nil, err
	}

	if input.LinkCurrentContact {
		if err = linkMutationContact(ctx, tx, input.OrgID, recordID, input.CurrentContactPhone); err != nil {
			return nil, err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO data_record_mutations
		(org_id,object_id,record_id,mutation_key,operation) VALUES($1::uuid,$2::uuid,$3::uuid,$4,$5)`,
		input.OrgID, objectID, recordID, input.IdempotencyKey, input.Operation); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &DataMutationResult{
		RecordID: recordID, ObjectKey: input.ObjectKey, Operation: input.Operation,
		Created: created, Idempotent: false, Data: raw,
	}, nil
}

func validateMutationFieldNames(fields []DataField, values map[string]any) error {
	allowed := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		allowed[field.Key] = struct{}{}
	}
	for key := range values {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("el campo %q no existe en el objeto", key)
		}
	}
	return nil
}

func fieldExists(fields []DataField, key string) bool {
	for _, field := range fields {
		if field.Key == key {
			return true
		}
	}
	return false
}

func mutationRecordByID(ctx context.Context, tx pgx.Tx, orgID, objectID, recordID string) (string, map[string]any, error) {
	var raw json.RawMessage
	err := tx.QueryRow(ctx, `SELECT r.id::text,r.data FROM data_records r
		JOIN data_objects o ON o.id=r.object_id
		WHERE r.id=$3::uuid AND r.object_id=$2::uuid AND o.org_id=$1::uuid FOR UPDATE`,
		orgID, objectID, recordID).Scan(&recordID, &raw)
	if err != nil {
		return "", nil, err
	}
	var values map[string]any
	if json.Unmarshal(raw, &values) != nil {
		return "", nil, fmt.Errorf("el registro contiene JSON inválido")
	}
	return recordID, values, nil
}

func mutationRecordByMatch(ctx context.Context, tx pgx.Tx, objectID, field, value string) (string, map[string]any, error) {
	rows, err := tx.Query(ctx, `SELECT id::text,data FROM data_records
		WHERE object_id=$1::uuid AND data->>$2=$3 ORDER BY created_at DESC LIMIT 2 FOR UPDATE`,
		objectID, field, value)
	if err != nil {
		return "", nil, err
	}
	type candidate struct {
		ID   string          `db:"id"`
		Data json.RawMessage `db:"data"`
	}
	candidates, err := pgx.CollectRows(rows, pgx.RowToStructByName[candidate])
	if err != nil {
		return "", nil, err
	}
	if len(candidates) == 0 {
		return "", nil, pgx.ErrNoRows
	}
	if len(candidates) > 1 {
		return "", nil, fmt.Errorf("upsert ambiguo: más de un registro coincide con %s", field)
	}
	var values map[string]any
	if json.Unmarshal(candidates[0].Data, &values) != nil {
		return "", nil, fmt.Errorf("el registro contiene JSON inválido")
	}
	return candidates[0].ID, values, nil
}

func linkMutationContact(ctx context.Context, tx pgx.Tx, orgID, recordID, phone string) error {
	phone = NormalizePhone(phone)
	var contactID string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM contacts
		WHERE org_id=$1::uuid AND phone_normalized=$2`, orgID, phone).Scan(&contactID); err != nil {
		if err == pgx.ErrNoRows {
			return fmt.Errorf("el contacto actual no existe en la organización")
		}
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM data_record_contacts
		WHERE record_id=$1::uuid AND role='primary' AND contact_id<>$2::uuid`, recordID, contactID); err != nil {
		return err
	}
	cmd, err := tx.Exec(ctx, `INSERT INTO data_record_contacts(record_id,contact_id,role)
		VALUES($1::uuid,$2::uuid,'primary') ON CONFLICT(record_id,contact_id,role) DO NOTHING`, recordID, contactID)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		// Ya vinculado es éxito idempotente.
		return nil
	}
	return nil
}

func previousDataMutation(ctx context.Context, tx pgx.Tx, orgID, objectID, objectKey, key string) (*DataMutationResult, bool, error) {
	var result DataMutationResult
	result.ObjectKey = objectKey
	err := tx.QueryRow(ctx, `SELECT m.record_id::text,m.operation,r.data
		FROM data_record_mutations m JOIN data_records r ON r.id=m.record_id
		WHERE m.org_id=$1::uuid AND m.object_id=$2::uuid AND m.mutation_key=$3`,
		orgID, objectID, key).Scan(&result.RecordID, &result.Operation, &result.Data)
	if err == pgx.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	result.Idempotent = true
	result.Created = false
	return &result, true, nil
}
