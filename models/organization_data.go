package models

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	if err := ValidateDataFieldDefinition(key, label, typ); err != nil {
		return nil, err
	}
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

// ListDataRecordsByOrg devuelve los registros con su contacto primario.
//
// El `LEFT JOIN` es la corrección del fallo por el que vincular un contacto
// parecía no guardarse: el vínculo se escribía en `data_record_contacts` pero
// esta consulta nunca lo leía, así que al recargar la pantalla el selector volvía
// a estar vacío. Es `LEFT` y no `INNER` porque un registro sin vincular tiene que
// seguir apareciendo: es justo el que el operador necesita ver antes de publicar
// un recordatorio.
func ListDataRecordsByOrg(ctx context.Context, p *pgxpool.Pool, orgID, objectID string) ([]DataRecordWithContact, error) {
	rows, e := p.Query(ctx, `SELECT r.id::text AS id,r.object_id::text AS object_id,r.data,
			c.id::text AS contact_id, c.name AS contact_name, c.phone_normalized AS contact_phone,
			r.created_at,r.updated_at
		FROM data_records r
		JOIN data_objects o ON o.id=r.object_id
		LEFT JOIN data_record_contacts rc ON rc.record_id=r.id AND rc.role='primary'
		LEFT JOIN contacts c ON c.id=rc.contact_id
		WHERE o.org_id=$1::uuid AND r.object_id=$2::uuid
		ORDER BY r.created_at DESC`, orgID, objectID)
	if e != nil {
		return nil, e
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[DataRecordWithContact])
}

// SearchDataRecordsByOrg busca registros de un objeto por su clave técnica.
//
// La consulta es un `ILIKE` sobre el JSON completo del registro, no una búsqueda
// por campo: quien la usa es un modelo que escribe términos sueltos ("tienda
// online", "sensores") sin saber en qué columna viven. Para un catálogo de
// decenas de filas es suficiente y es honesto; si algún día se aplica a miles de
// registros habrá que cambiarlo por un índice de texto, no por más `ILIKE`.
//
// El objeto se resuelve **por clave dentro de la organización**, que es el límite
// de alcance: un agente configurado sobre `servicios` no puede leer `facturas`.
func SearchDataRecordsByOrg(ctx context.Context, p *pgxpool.Pool, orgID, objectKey, query string, limit int) ([]DataRecord, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, e := p.Query(ctx, `SELECT `+dataRecordCols+`
		FROM data_records r
		WHERE r.object_id = (SELECT id FROM data_objects WHERE org_id=$1::uuid AND key=$2)
		  AND ($3 = '' OR r.data::text ILIKE '%' || $3 || '%')
		ORDER BY r.created_at DESC
		LIMIT $4`, orgID, objectKey, strings.TrimSpace(query), limit)
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

// LinkRecordContactByOrg vincula un contacto a un registro.
//
// El `ON CONFLICT` nombra las tres columnas de la clave primaria de
// `data_record_contacts`: con menos, Postgres rechaza la sentencia entera con
// 42P10 porque ningún índice único coincide con la especificación.
//
// Elegir **otro** contacto reemplaza al anterior en la misma transacción. Sin
// eso, la clave `(record_id, contact_id, role)` deja convivir dos `primary` para
// el mismo registro y `PrimaryContactForRecord` acabaría eligiendo uno de los dos:
// el recordatorio se iría a quien no toca, y sin patrón. Un registro tiene un
// destinatario, no una colección.
func LinkRecordContactByOrg(ctx context.Context, p *pgxpool.Pool, orgID, recordID, contactID, role string) error {
	tx, e := p.Begin(ctx)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if role == "primary" {
		if _, e := tx.Exec(ctx, `DELETE FROM data_record_contacts rc
			USING data_records r JOIN data_objects o ON o.id=r.object_id
			WHERE rc.record_id=r.id AND r.id=$2::uuid AND o.org_id=$1::uuid
			  AND rc.role='primary' AND rc.contact_id<>$3::uuid`, orgID, recordID, contactID); e != nil {
			return e
		}
	}
	cmd, e := tx.Exec(ctx, `INSERT INTO data_record_contacts(record_id,contact_id,role) SELECT r.id,c.id,$4 FROM data_records r JOIN data_objects o ON o.id=r.object_id JOIN contacts c ON c.org_id=o.org_id WHERE r.id=$2::uuid AND c.id=$3::uuid AND o.org_id=$1::uuid ON CONFLICT (record_id,contact_id,role) DO UPDATE SET role=EXCLUDED.role`, orgID, recordID, contactID, role)
	if e != nil {
		return e
	}
	if cmd.RowsAffected() == 0 {
		return fmt.Errorf("registro o contacto no pertenece a la organización")
	}
	return tx.Commit(ctx)
}

// UnlinkRecordContactByOrg deshace el vínculo. Existe porque sin él un contacto
// mal elegido no se puede corregir a "ninguno": solo sustituir por otro.
func UnlinkRecordContactByOrg(ctx context.Context, p *pgxpool.Pool, orgID, recordID, contactID string) error {
	cmd, e := p.Exec(ctx, `DELETE FROM data_record_contacts rc
		USING data_records r JOIN data_objects o ON o.id=r.object_id
		WHERE rc.record_id=r.id AND r.id=$2::uuid AND rc.contact_id=$3::uuid AND o.org_id=$1::uuid`,
		orgID, recordID, contactID)
	if e != nil {
		return e
	}
	if cmd.RowsAffected() == 0 {
		return fmt.Errorf("el registro no estaba vinculado a ese contacto")
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
	fields, err := ListDataFieldsByOrg(ctx, p, orgID, objectID)
	if err != nil {
		return nil, err
	}
	if err = ValidateDataFilter(fields, filter); err != nil {
		return nil, err
	}
	rows, e := p.Query(ctx, `INSERT INTO data_views(object_id,name,filter) SELECT o.id,$3,$4::jsonb FROM data_objects o WHERE o.id=$2::uuid AND o.org_id=$1::uuid RETURNING `+dataViewCols, orgID, objectID, name, filter)
	if e != nil {
		return nil, e
	}
	v, e := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[DataView])
	return &v, e
}

func UpdateDataViewByOrg(ctx context.Context, p *pgxpool.Pool, orgID, objectID, viewID, name string, filter json.RawMessage) (*DataView, error) {
	if len(filter) == 0 {
		filter = json.RawMessage(`{}`)
	}
	fields, err := ListDataFieldsByOrg(ctx, p, orgID, objectID)
	if err != nil {
		return nil, err
	}
	if err = ValidateDataFilter(fields, filter); err != nil {
		return nil, err
	}
	rows, err := p.Query(ctx, `UPDATE data_views SET name=$4,filter=$5::jsonb,updated_at=NOW()
		WHERE id=$3::uuid AND object_id=(SELECT id FROM data_objects WHERE id=$2::uuid AND org_id=$1::uuid)
		RETURNING `+dataViewCols, orgID, objectID, viewID, name, filter)
	if err != nil {
		return nil, err
	}
	v, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[DataView])
	return &v, err
}

func DeleteDataViewByOrg(ctx context.Context, p *pgxpool.Pool, orgID, objectID, viewID string) (bool, error) {
	cmd, err := p.Exec(ctx, `DELETE FROM data_views
		WHERE id=$3::uuid AND object_id=(SELECT id FROM data_objects WHERE id=$2::uuid AND org_id=$1::uuid)`,
		orgID, objectID, viewID)
	if err != nil {
		return false, err
	}
	return cmd.RowsAffected() > 0, nil
}
