package models

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ExternalConnection es a quién llama la organización cuando un bloque consulta
// un sistema externo en directo.
//
// La credencial nunca sale de aquí en claro: se guarda cifrada y quien la
// necesita la descifra en la capa que tiene el Cipher, igual que el token del
// canal. `json:"-"` no es decorativo — sin él, cualquier endpoint que devuelva
// la fila publicaría la clave de la tienda.
type ExternalConnection struct {
	ID            string     `db:"id"            json:"id"`
	OrgID         string     `db:"org_id"        json:"orgId"`
	Key           string     `db:"key"           json:"key"`
	Driver        string     `db:"driver"        json:"driver"`
	Label         string     `db:"label"         json:"label"`
	BaseURL       string     `db:"base_url"      json:"baseUrl"`
	CredentialEnc []byte     `db:"credential"    json:"-"`
	Status        string     `db:"status"        json:"status"`
	LastOKAt      *time.Time `db:"last_ok_at"    json:"lastOkAt,omitempty"`
	LastError     *string    `db:"last_error"    json:"lastError,omitempty"`
	CreatedAt     time.Time  `db:"created_at"    json:"createdAt"`
	UpdatedAt     time.Time  `db:"updated_at"    json:"updatedAt"`
}

// Active responde si la conexión puede usarse en una conversación.
func (c ExternalConnection) Active() bool { return c.Status == "active" }

const externalConnectionCols = `id::text AS id, org_id::text AS org_id, key, driver, label,
	base_url, credential, status, last_ok_at, last_error, created_at, updated_at`

// ExternalConnectionInput es el alta o la actualización de una conexión.
type ExternalConnectionInput struct {
	OrgID   string
	Key     string
	Driver  string
	Label   string
	BaseURL string
	// CredentialEnc vacío en una actualización conserva la credencial guardada.
	// Es lo que permite renombrar una conexión sin volver a pedir la clave.
	CredentialEnc []byte
	Status        string
}

// SaveExternalConnection crea o actualiza la conexión de una organización.
//
// La identidad es (org_id, key) y no el id: el flujo referencia la clave
// técnica, así que dos altas seguidas de "meudim" tienen que ser la misma
// conexión actualizada, no dos filas compitiendo por el mismo nombre.
func SaveExternalConnection(ctx context.Context, pool *pgxpool.Pool, input ExternalConnectionInput) (*ExternalConnection, error) {
	key := strings.TrimSpace(input.Key)
	if input.OrgID == "" || key == "" || strings.TrimSpace(input.Driver) == "" {
		return nil, errors.New("la conexión requiere organización, clave y driver")
	}
	label := strings.TrimSpace(input.Label)
	if label == "" {
		label = key
	}
	status := strings.TrimSpace(input.Status)
	if status == "" {
		status = "active"
	}
	if len(input.CredentialEnc) == 0 {
		// El INSERT sí la exige: una conexión nueva sin credencial no serviría
		// para nada y la columna es NOT NULL.
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT true FROM external_connections WHERE org_id = $1::uuid AND key = $2`,
			input.OrgID, key).Scan(&exists); err != nil {
			return nil, errors.New("la conexión nueva requiere una credencial")
		}
	}
	rows, err := pool.Query(ctx,
		`INSERT INTO external_connections (org_id, key, driver, label, base_url, credential, status)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (org_id, key) DO UPDATE SET
		     driver     = EXCLUDED.driver,
		     label      = EXCLUDED.label,
		     base_url   = EXCLUDED.base_url,
		     credential = COALESCE(NULLIF(EXCLUDED.credential, ''::bytea), external_connections.credential),
		     status     = EXCLUDED.status
		 RETURNING `+externalConnectionCols,
		input.OrgID, key, strings.TrimSpace(input.Driver), label,
		strings.TrimSpace(input.BaseURL), input.CredentialEnc, status)
	if err != nil {
		return nil, err
	}
	connection, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[ExternalConnection])
	if err != nil {
		return nil, err
	}
	return &connection, nil
}

// ExternalConnectionByKey resuelve la conexión que nombra un bloque del flujo.
// Devuelve (nil, nil) si la organización no tiene ninguna con esa clave: que
// falte es un caso de configuración, no un fallo de la base.
func ExternalConnectionByKey(ctx context.Context, pool *pgxpool.Pool, orgID, key string) (*ExternalConnection, error) {
	rows, err := pool.Query(ctx,
		`SELECT `+externalConnectionCols+`
		 FROM external_connections WHERE org_id = $1::uuid AND key = $2`,
		orgID, strings.TrimSpace(key))
	if err != nil {
		return nil, err
	}
	connection, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[ExternalConnection])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &connection, nil
}

// ListExternalConnections devuelve las de una organización, para el panel.
func ListExternalConnections(ctx context.Context, pool *pgxpool.Pool, orgID string) ([]ExternalConnection, error) {
	rows, err := pool.Query(ctx,
		`SELECT `+externalConnectionCols+`
		 FROM external_connections WHERE org_id = $1::uuid ORDER BY key`, orgID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ExternalConnection])
}

// DeleteExternalConnection borra la conexión. Los bloques que la referencian
// dejarán de funcionar y saldrán por su rama de error: es lo correcto, porque
// desactivar una tienda no debe hacer que el bot invente precios.
func DeleteExternalConnection(ctx context.Context, pool *pgxpool.Pool, orgID, key string) (bool, error) {
	tag, err := pool.Exec(ctx,
		`DELETE FROM external_connections WHERE org_id = $1::uuid AND key = $2`,
		orgID, strings.TrimSpace(key))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// RecordExternalConnectionResult anota cómo fue la última llamada.
//
// Es observabilidad, no caché: aquí no se guarda ni un producto ni un precio.
// Un acierto limpia el error anterior, para que el panel no muestre para siempre
// un fallo que ya se resolvió.
func RecordExternalConnectionResult(ctx context.Context, pool *pgxpool.Pool, id string, callErr error) error {
	if callErr == nil {
		_, err := pool.Exec(ctx,
			`UPDATE external_connections SET last_ok_at = NOW(), last_error = NULL WHERE id = $1::uuid`, id)
		return err
	}
	message := callErr.Error()
	if len(message) > 500 {
		message = message[:500]
	}
	_, err := pool.Exec(ctx,
		`UPDATE external_connections SET last_error = $2 WHERE id = $1::uuid`, id, message)
	return err
}

// MaskCredential deja visible lo justo para reconocer una clave sin revelarla:
// su prefijo (pk_live_, sk_live_…) y los últimos cuatro caracteres. Es la misma
// convención que usa el panel de Meudim.
func MaskCredential(credential string) string {
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return ""
	}
	if len(credential) <= 8 {
		return strings.Repeat("•", len(credential))
	}
	prefix := ""
	if parts := strings.SplitN(credential, "_", 3); len(parts) == 3 {
		prefix = parts[0] + "_" + parts[1] + "_"
	}
	return prefix + "…" + credential[len(credential)-4:]
}
