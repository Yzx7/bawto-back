package models

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func ListDataObjectsByOrg(ctx context.Context, p *pgxpool.Pool, orgID string) ([]DataObject, error) {
	rows, e := p.Query(ctx, `SELECT `+dataObjectCols+` FROM data_objects WHERE org_id=$1::uuid ORDER BY created_at`, orgID)
	if e != nil {
		return nil, e
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[DataObject])
}
func CreateDataObjectByOrg(ctx context.Context, p *pgxpool.Pool, orgID, key, name, plural string) (*DataObject, error) {
	rows, e := p.Query(ctx, `INSERT INTO data_objects(org_id,key,name,plural_name) VALUES($1::uuid,$2,$3,$4) RETURNING `+dataObjectCols, orgID, key, name, plural)
	if e != nil {
		return nil, e
	}
	v, e := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[DataObject])
	return &v, e
}
func ListDataFieldsByOrg(ctx context.Context, p *pgxpool.Pool, orgID, objectID string) ([]DataField, error) {
	rows, e := p.Query(ctx, `SELECT f.id::text AS id,f.object_id::text AS object_id,f.key,f.label,f.type,f.required,f.created_at,f.updated_at FROM data_fields f JOIN data_objects o ON o.id=f.object_id WHERE o.org_id=$1::uuid AND f.object_id=$2::uuid ORDER BY f.created_at`, orgID, objectID)
	if e != nil {
		return nil, e
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[DataField])
}
func UpsertDataFieldByOrg(ctx context.Context, p *pgxpool.Pool, orgID, objectID, key, label, typ string, required bool) (*DataField, error) {
	rows, e := p.Query(ctx, `INSERT INTO data_fields(object_id,key,label,type,required) SELECT o.id,$3,$4,$5,$6 FROM data_objects o WHERE o.id=$2::uuid AND o.org_id=$1::uuid ON CONFLICT(object_id,key) DO UPDATE SET label=EXCLUDED.label,type=EXCLUDED.type,required=EXCLUDED.required RETURNING `+dataFieldCols, orgID, objectID, key, label, typ, required)
	if e != nil {
		return nil, e
	}
	v, e := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[DataField])
	return &v, e
}
func CreateDataRecordByOrg(ctx context.Context, p *pgxpool.Pool, orgID, objectID string, data json.RawMessage) (*DataRecord, error) {
	fields, e := ListDataFieldsByOrg(ctx, p, orgID, objectID)
	if e != nil {
		return nil, e
	}
	if e = ValidateDataRecord(fields, data); e != nil {
		return nil, e
	}
	rows, e := p.Query(ctx, `INSERT INTO data_records(object_id,data) SELECT id,$3::jsonb FROM data_objects WHERE id=$2::uuid AND org_id=$1::uuid RETURNING `+dataRecordCols, orgID, objectID, data)
	if e != nil {
		return nil, e
	}
	v, e := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[DataRecord])
	return &v, e
}
func ListDataRecordsByOrg(ctx context.Context, p *pgxpool.Pool, orgID, objectID string) ([]DataRecord, error) {
	rows, e := p.Query(ctx, `SELECT r.id::text AS id,r.object_id::text AS object_id,r.data,r.created_at,r.updated_at FROM data_records r JOIN data_objects o ON o.id=r.object_id WHERE o.org_id=$1::uuid AND r.object_id=$2::uuid ORDER BY r.created_at DESC`, orgID, objectID)
	if e != nil {
		return nil, e
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[DataRecord])
}
func ListContactsByOrg(ctx context.Context, p *pgxpool.Pool, orgID string) ([]Contact, error) {
	rows, e := p.Query(ctx, `SELECT `+contactCols+` FROM contacts WHERE org_id=$1::uuid ORDER BY created_at DESC`, orgID)
	if e != nil {
		return nil, e
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Contact])
}

func ListContactFieldsByOrg(ctx context.Context, p *pgxpool.Pool, orgID string) ([]ContactField, error) {
	rows, e := p.Query(ctx, `SELECT `+fieldCols+` FROM contact_fields WHERE org_id=$1::uuid ORDER BY created_at`, orgID)
	if e != nil {
		return nil, e
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ContactField])
}

func UpsertContactFieldByOrg(ctx context.Context, p *pgxpool.Pool, orgID, key, label, typ string, required bool) (*ContactField, error) {
	rows, e := p.Query(ctx, `INSERT INTO contact_fields (org_id,key,label,type,required)
		VALUES ($1::uuid,$2,$3,$4,$5)
		ON CONFLICT (org_id,key) DO UPDATE SET label=EXCLUDED.label,type=EXCLUDED.type,required=EXCLUDED.required,updated_at=NOW()
		RETURNING `+fieldCols, orgID, key, label, typ, required)
	if e != nil {
		return nil, e
	}
	v, e := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[ContactField])
	return &v, e
}

// SaveContactByOrg crea o actualiza una identidad de canal sin depender de que
// la organización ya tenga un bot. Un ID vacío hace upsert por teléfono.
func SaveContactByOrg(ctx context.Context, p *pgxpool.Pool, orgID, contactID, phone, name, status string, data json.RawMessage) (*Contact, error) {
	phone = NormalizePhone(phone)
	if len(phone) < 6 || len(phone) > 20 {
		return nil, fmt.Errorf("teléfono inválido")
	}
	if status == "" {
		status = "active"
	}
	if status != "active" && status != "inactive" && status != "blocked" {
		return nil, fmt.Errorf("estado inválido")
	}
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	fields, e := ListContactFieldsByOrg(ctx, p, orgID)
	if e != nil {
		return nil, e
	}
	if e = ValidateContactData(fields, data); e != nil {
		return nil, e
	}

	var rows pgx.Rows
	if contactID == "" {
		rows, e = p.Query(ctx, `INSERT INTO contacts (org_id,phone_normalized,name,status,data)
			VALUES ($1::uuid,$2,NULLIF($3,''),$4,$5::jsonb)
			ON CONFLICT (org_id,phone_normalized) DO UPDATE SET
			name=COALESCE(NULLIF(EXCLUDED.name,''),contacts.name),status=EXCLUDED.status,data=EXCLUDED.data,updated_at=NOW()
			RETURNING `+contactCols, orgID, phone, name, status, data)
	} else {
		rows, e = p.Query(ctx, `UPDATE contacts SET phone_normalized=$3,name=NULLIF($4,''),status=$5,data=$6::jsonb,updated_at=NOW()
			WHERE org_id=$1::uuid AND id=$2::uuid RETURNING `+contactCols, orgID, contactID, phone, name, status, data)
	}
	if e != nil {
		return nil, e
	}
	v, e := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Contact])
	if e != nil {
		return nil, e
	}
	return &v, nil
}
func LinkRecordContactByOrg(ctx context.Context, p *pgxpool.Pool, orgID, recordID, contactID, role string) error {
	cmd, e := p.Exec(ctx, `INSERT INTO data_record_contacts(record_id,contact_id,role) SELECT r.id,c.id,$4 FROM data_records r JOIN data_objects o ON o.id=r.object_id JOIN contacts c ON c.org_id=o.org_id WHERE r.id=$2::uuid AND c.id=$3::uuid AND o.org_id=$1::uuid ON CONFLICT(record_id,contact_id) DO UPDATE SET role=EXCLUDED.role`, orgID, recordID, contactID, role)
	if e != nil {
		return e
	}
	if cmd.RowsAffected() == 0 {
		return fmt.Errorf("registro o contacto no pertenece a la organización")
	}
	return nil
}
func ListDataViewsByOrg(ctx context.Context, p *pgxpool.Pool, orgID, objectID string) ([]DataView, error) {
	rows, e := p.Query(ctx, `SELECT v.id::text AS id,v.object_id::text AS object_id,v.name,v.filter,v.created_at,v.updated_at FROM data_views v JOIN data_objects o ON o.id=v.object_id WHERE o.org_id=$1::uuid AND v.object_id=$2::uuid ORDER BY v.created_at`, orgID, objectID)
	if e != nil {
		return nil, e
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[DataView])
}
func CreateDataViewByOrg(ctx context.Context, p *pgxpool.Pool, orgID, objectID, name string, filter json.RawMessage) (*DataView, error) {
	if len(filter) == 0 {
		filter = json.RawMessage(`{}`)
	}
	rows, e := p.Query(ctx, `INSERT INTO data_views(object_id,name,filter) SELECT o.id,$3,$4::jsonb FROM data_objects o WHERE o.id=$2::uuid AND o.org_id=$1::uuid RETURNING `+dataViewCols, orgID, objectID, name, filter)
	if e != nil {
		return nil, e
	}
	v, e := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[DataView])
	return &v, e
}
