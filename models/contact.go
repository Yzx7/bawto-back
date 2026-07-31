package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Contact es el CRM mínimo por bot. Data contiene campos que cada organización
// define (plan, zona, documento, etc.) sin crear tablas por cliente.
type Contact struct {
	ID              string          `db:"id" json:"id"`
	OrgID           string          `db:"org_id" json:"orgId"`
	PhoneNormalized string          `db:"phone_normalized" json:"phoneNormalized"`
	Name            *string         `db:"name" json:"name,omitempty"`
	Data            json.RawMessage `db:"data" json:"data"`
	Status          string          `db:"status" json:"status"`
	CreatedAt       time.Time       `db:"created_at" json:"createdAt"`
	UpdatedAt       time.Time       `db:"updated_at" json:"updatedAt"`
}

type BillingRecord struct {
	ID        string          `db:"id" json:"id"`
	ContactID string          `db:"contact_id" json:"contactId"`
	Period    string          `db:"period" json:"period"`
	Amount    string          `db:"amount" json:"amount"`
	Currency  string          `db:"currency" json:"currency"`
	DueDate   time.Time       `db:"due_date" json:"dueDate"`
	Status    string          `db:"status" json:"status"`
	PaidAt    *time.Time      `db:"paid_at" json:"paidAt,omitempty"`
	Evidence  json.RawMessage `db:"evidence" json:"evidence"`
	CreatedAt time.Time       `db:"created_at" json:"createdAt"`
	UpdatedAt time.Time       `db:"updated_at" json:"updatedAt"`
}

const contactCols = `id::text AS id, org_id::text AS org_id, phone_normalized, name, data, status, created_at, updated_at`

// contactColsC es la misma lista calificada con el alias `c`. Toda consulta con
// JOIN debe usar esta: `created_at` existe también en `audience_contacts`, en
// `data_records` y en `data_objects`, y con la lista sin prefijo Postgres
// responde "column reference is ambiguous" en tiempo de ejecución, no al
// compilar. Es el mismo tropiezo que ya costó `GetFlowVersion` y
// `InvalidDateRecordsForView`.
const contactColsC = `c.id::text AS id, c.org_id::text AS org_id, c.phone_normalized, c.name, c.data, c.status, c.created_at, c.updated_at`
const billingCols = `id::text AS id, contact_id::text AS contact_id, period, amount::text AS amount, currency, due_date, status, paid_at, evidence, created_at, updated_at`

// NormalizePhone deja solo los dígitos. La API y los webhooks deben usarla.
func NormalizePhone(phone string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) {
			return r
		}
		return -1
	}, phone)
}

// EnsureInboundContact registra la identidad observada por el canal sin borrar
// atributos CRM que ya hubiera configurado la organización.
func EnsureInboundContact(ctx context.Context, pool *pgxpool.Pool, botID, phone, name string) (*Contact, error) {
	phone = NormalizePhone(phone)
	if len(phone) < 6 || len(phone) > 20 {
		return nil, fmt.Errorf("teléfono inválido")
	}
	rows, err := pool.Query(ctx, `INSERT INTO contacts(org_id,phone_normalized,name)
		SELECT org_id,$2,NULLIF($3,'') FROM bots WHERE id=$1::uuid
		ON CONFLICT(org_id,phone_normalized) DO UPDATE SET name=COALESCE(NULLIF(EXCLUDED.name,''),contacts.name),updated_at=NOW()
		RETURNING `+contactCols, botID, phone, name)
	if err != nil {
		return nil, err
	}
	v, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Contact])
	return &v, err
}

// ValidateContactData aplica el contrato configurado para el bot. Las claves
// no declaradas se mantienen: el CSV puede traer información antes de que el
// administrador decida formalizarla como campo.
func ValidateContactData(fields []ContactField, data json.RawMessage) error {
	values := map[string]any{}
	if !json.Valid(data) || json.Unmarshal(data, &values) != nil {
		return fmt.Errorf("data debe ser un objeto JSON")
	}
	for _, field := range fields {
		value, exists := values[field.Key]
		if !exists || value == nil || strings.TrimSpace(fmt.Sprint(value)) == "" {
			if field.Required {
				return fmt.Errorf("el campo %q es obligatorio", field.Label)
			}
			continue
		}
		text := fmt.Sprint(value)
		switch field.Type {
		case "number":
			if _, err := strconv.ParseFloat(text, 64); err != nil {
				return fmt.Errorf("el campo %q debe ser numérico", field.Label)
			}
		case "date":
			if _, err := time.Parse("2006-01-02", text); err != nil {
				return fmt.Errorf("el campo %q debe tener formato AAAA-MM-DD", field.Label)
			}
		case "boolean":
			if _, err := strconv.ParseBool(text); err != nil {
				return fmt.Errorf("el campo %q debe ser verdadero o falso", field.Label)
			}
		}
	}
	return nil
}

func GetContactByPhone(ctx context.Context, pool *pgxpool.Pool, botID, phone string) (*Contact, error) {
	rows, err := pool.Query(ctx, `SELECT `+contactCols+` FROM contacts WHERE org_id = (SELECT org_id FROM bots WHERE id=$1::uuid) AND phone_normalized = $2`, botID, NormalizePhone(phone))
	if err != nil {
		return nil, err
	}
	c, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Contact])
	if err != nil {
		return nil, nil
	}
	return &c, nil
}

func GetContact(ctx context.Context, pool *pgxpool.Pool, botID, contactID string) (*Contact, error) {
	rows, err := pool.Query(ctx, `SELECT `+contactCols+` FROM contacts WHERE org_id=(SELECT org_id FROM bots WHERE id=$1::uuid) AND id=$2::uuid`, botID, contactID)
	if err != nil {
		return nil, err
	}
	c, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Contact])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func ListBillingByContact(ctx context.Context, pool *pgxpool.Pool, botID, contactID string) ([]BillingRecord, error) {
	rows, err := pool.Query(ctx, `SELECT b.id::text AS id, b.contact_id::text AS contact_id, b.period, b.amount::text AS amount, b.currency, b.due_date, b.status, b.paid_at, b.evidence, b.created_at, b.updated_at
        FROM billing_records b JOIN contacts c ON c.id = b.contact_id
		WHERE c.org_id = (SELECT org_id FROM bots WHERE id=$1::uuid) AND b.contact_id = $2::uuid ORDER BY b.due_date DESC`, botID, contactID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[BillingRecord])
}

func CreateBillingRecord(ctx context.Context, pool *pgxpool.Pool, botID, contactID, period, amount, currency, dueDate string) (*BillingRecord, error) {
	if currency == "" {
		currency = "PEN"
	}
	rows, err := pool.Query(ctx, `INSERT INTO billing_records (contact_id, period, amount, currency, due_date)
		SELECT c.id, $3, $4::numeric, $5, $6::date FROM contacts c WHERE c.id = $2::uuid AND c.org_id = (SELECT org_id FROM bots WHERE id=$1::uuid)
        RETURNING `+billingCols, botID, contactID, period, amount, currency, dueDate)
	if err != nil {
		return nil, err
	}
	b, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[BillingRecord])
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// FlowContext incluye identidad de canal y los registros genéricos vinculados
// al contacto. Así un flujo entrante puede usar {data_<objeto>_<campo>}.
func FlowContext(ctx context.Context, pool *pgxpool.Pool, botID, phone string) map[string]string {
	contact, err := GetContactByPhone(ctx, pool, botID, phone)
	if err != nil || contact == nil {
		return map[string]string{}
	}
	vars := map[string]string{"contact_id": contact.ID, "contact_phone": contact.PhoneNormalized, "contact_status": contact.Status}
	if contact.Name != nil {
		vars["contact_name"] = *contact.Name
	}
	var fields map[string]any
	if json.Unmarshal(contact.Data, &fields) == nil {
		for key, value := range fields {
			if s, ok := value.(string); ok {
				vars["contact_"+key] = s
			}
		}
	}
	rows, err := pool.Query(ctx, `SELECT o.key,r.data FROM data_record_contacts rc JOIN data_records r ON r.id=rc.record_id JOIN data_objects o ON o.id=r.object_id WHERE rc.contact_id=$1::uuid AND o.org_id=(SELECT org_id FROM bots WHERE id=$2::uuid) ORDER BY r.created_at DESC`, contact.ID, botID)
	if err != nil {
		return vars
	}
	defer rows.Close()
	for rows.Next() {
		var objectKey string
		var raw json.RawMessage
		if err := rows.Scan(&objectKey, &raw); err != nil {
			continue
		}
		var record map[string]any
		if json.Unmarshal(raw, &record) != nil {
			continue
		}
		for key, value := range record {
			if value != nil {
				variable := "data_" + objectKey + "_" + key
				if _, exists := vars[variable]; !exists {
					vars[variable] = fmt.Sprint(value)
				}
			}
		}
	}
	return vars
}

// RecordPaymentReceipt asocia la media durable del mensaje con la factura más
// reciente vinculada al contacto y cambia su estado a validación.
func RecordPaymentReceipt(ctx context.Context, pool *pgxpool.Pool, botID, phone, waID, mediaID, mimeType string) (string, error) {
	return RecordPaymentReceiptForRecord(ctx, pool, botID, phone, waID, mediaID, mimeType, "")
}

// RecordPaymentReceiptForRecord usa primero la correlación genérica del
// recordatorio. El fallback legado por objeto `facturas` se conserva mientras
// el prototipo billing_records se migra, pero nunca reemplaza un ID explícito.
func RecordPaymentReceiptForRecord(ctx context.Context, pool *pgxpool.Pool, botID, phone, waID, mediaID, mimeType, preferredRecordID string) (string, error) {
	contact, err := GetContactByPhone(ctx, pool, botID, phone)
	if err != nil || contact == nil {
		return "", fmt.Errorf("contacto no encontrado")
	}
	if mediaID == "" || waID == "" {
		return "", fmt.Errorf("comprobante sin media")
	}
	var stored bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM messages m JOIN message_media mm ON mm.message_id=m.id WHERE m.wa_id=$1 AND mm.provider_id=$2)`, waID, mediaID).Scan(&stored); err != nil || !stored {
		return "", fmt.Errorf("no se pudo conservar la imagen del comprobante")
	}
	var recordID string
	if preferredRecordID != "" {
		err = pool.QueryRow(ctx, `SELECT r.id::text FROM data_records r
			JOIN data_objects o ON o.id=r.object_id
			JOIN data_record_contacts rc ON rc.record_id=r.id
			WHERE r.id=$1::uuid AND rc.contact_id=$2::uuid
			  AND o.org_id=(SELECT org_id FROM bots WHERE id=$3::uuid)`,
			preferredRecordID, contact.ID, botID).Scan(&recordID)
	} else {
		err = pool.QueryRow(ctx, `SELECT r.id::text FROM data_records r
			JOIN data_objects o ON o.id=r.object_id
			JOIN data_record_contacts rc ON rc.record_id=r.id
			WHERE rc.contact_id=$1::uuid AND o.org_id=(SELECT org_id FROM bots WHERE id=$2::uuid)
			  AND o.key IN ('facturas','factura')
			  AND COALESCE(r.data->>'estado','') NOT IN ('pagado','cancelado')
			ORDER BY CASE WHEN r.data->>'estado' IN ('pendiente','vencido') THEN 0 ELSE 1 END,r.created_at DESC LIMIT 1`, contact.ID, botID).Scan(&recordID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("no hay una factura pendiente vinculada al contacto")
	}
	if err != nil {
		return "", err
	}
	_, err = pool.Exec(ctx, `UPDATE data_records SET data=data || jsonb_build_object(
		'estado','validacion','comprobante_message_id',$2,'comprobante_media_id',$3,
		'comprobante_mime',$4,'comprobante_recibido_at',NOW()::text),updated_at=NOW()
		WHERE id=$1::uuid`, recordID, waID, mediaID, mimeType)
	if err != nil {
		return "", err
	}
	return recordID, nil
}

// PendingBillingForContact devuelve el cobro más urgente que aún requiere pago.
func PendingBillingForContact(ctx context.Context, pool *pgxpool.Pool, contactID string) (*BillingRecord, error) {
	rows, err := pool.Query(ctx, `SELECT `+billingCols+` FROM billing_records
        WHERE contact_id = $1::uuid AND status IN ('pending','overdue')
        ORDER BY due_date ASC LIMIT 1`, contactID)
	if err != nil {
		return nil, err
	}
	b, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[BillingRecord])
	if err != nil {
		return nil, nil
	}
	return &b, nil
}
