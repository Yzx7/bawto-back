package models

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Credenciales del servicio MCP de autoría. Una key da acceso a los flujos de
// **una** organización y, opcionalmente, solo a algunos de ellos.
//
// Sustituyen a los tokens HMAC autofirmados: aquellos no se podían revocar sin
// rotar la clave del servidor —o sea, sin invalidar los de todo el mundo—, y el
// alcance viajaba dentro del propio token, así que auditarlo obligaba a
// descifrar cada credencial repartida. Aquí la autoridad vive en una fila.

// MCPKeyPrefix marca las keys de este servicio. Existe para que se reconozcan a
// simple vista en un log, un `.env` o un escáner de secretos: una cadena
// aleatoria suelta no dice a quién avisar cuando aparece donde no debe.
const MCPKeyPrefix = "bwmcp_"

// mcpKeySecretBytes son los bytes aleatorios del secreto. 32 bytes = 256 bits:
// el mismo margen que una clave simétrica, y muy por encima de lo que hace falta
// para que adivinarla sea imposible aunque el atacante conozca el formato.
const mcpKeySecretBytes = 32

// mcpKeyPrefixVisible es cuánto de la key se guarda en claro para poder
// distinguirlas en el panel. Revela unos pocos bits de un secreto de 256, que es
// el precio de que revocar «la correcta» no sea adivinar entre filas idénticas.
const mcpKeyPrefixVisible = 6

// mcpKeyTouchInterval es cada cuánto se refresca `last_used_at`. Sin este
// margen, cada llamada del MCP —y validar un grafo se llama muchas veces
// seguidas— convertiría una lectura en una escritura. Para decidir si una key
// sigue en uso basta la resolución de minutos.
const mcpKeyTouchInterval = 5 * time.Minute

// ErrMCPKeyInvalidScope distingue un alcance mal formado de un fallo de base.
var ErrMCPKeyInvalidScope = errors.New("el alcance debe ser una lista de ids de flujo de esta organización")

// MCPKey es una credencial **sin su secreto**: de la key solo sobrevive el hash
// y el prefijo visible. La cadena completa existe una única vez, en la respuesta
// de CreateMCPKey.
type MCPKey struct {
	ID         string     `db:"id" json:"id"`
	OrgID      string     `db:"org_id" json:"orgId"`
	Name       string     `db:"name" json:"name"`
	KeyPrefix  string     `db:"key_prefix" json:"keyPrefix"`
	FlowIDs    []string   `db:"flow_ids" json:"flowIds"`
	CreatedBy  *string    `db:"created_by" json:"createdBy,omitempty"`
	CreatedAt  time.Time  `db:"created_at" json:"createdAt"`
	ExpiresAt  *time.Time `db:"expires_at" json:"expiresAt,omitempty"`
	LastUsedAt *time.Time `db:"last_used_at" json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time `db:"revoked_at" json:"revokedAt,omitempty"`
	RevokedBy  *string    `db:"revoked_by" json:"revokedBy,omitempty"`
}

// Scoped indica si la key está acotada a flujos concretos. Vacío significa toda
// la organización, que es el caso por defecto al crear sin lista.
func (k *MCPKey) Scoped() bool { return k != nil && len(k.FlowIDs) > 0 }

// AllowsFlow decide si la key alcanza un flujo. Es la única regla de alcance y
// vive junto a los datos que la sostienen, no repartida por los llamadores.
func (k *MCPKey) AllowsFlow(flowID string) bool {
	if k == nil {
		return false
	}
	if !k.Scoped() {
		return true
	}
	for _, allowed := range k.FlowIDs {
		if allowed == flowID {
			return true
		}
	}
	return false
}

// Active resume las dos formas de que una key deje de valer.
func (k *MCPKey) Active(now time.Time) bool {
	if k == nil || k.RevokedAt != nil {
		return false
	}
	return k.ExpiresAt == nil || now.Before(*k.ExpiresAt)
}

// El array vuelve como text[] porque el destino es []string: pedirle a pgx que
// convierta uuid[] obligaría a registrar el tipo en cada pool que use el modelo.
const mcpKeyCols = `id::text AS id, org_id::text AS org_id, name, key_prefix,
	flow_ids::text[] AS flow_ids, created_by, created_at, expires_at,
	last_used_at, revoked_at, revoked_by`

// NewMCPKeySecret genera una key nueva y devuelve la cadena completa, su prefijo
// visible y su hash. La cadena no se guarda en ninguna parte: quien la reciba es
// el único que volverá a tenerla.
func NewMCPKeySecret() (key, prefix, hash string, err error) {
	raw := make([]byte, mcpKeySecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", "", fmt.Errorf("generar la key: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	key = MCPKeyPrefix + secret
	return key, MCPKeyPrefix + secret[:mcpKeyPrefixVisible], HashMCPKey(key), nil
}

// HashMCPKey es SHA-256 en hexadecimal. No lleva salt ni iteraciones y no es un
// descuido: la entrada es un secreto de 256 bits generado por el servidor, así
// que no hay diccionario que precomputar ni contraseña que derivar. Un KDF lento
// aquí cobraría su coste en **cada** petición del MCP sin añadir nada frente a
// un atacante que no puede adivinar la entrada.
func HashMCPKey(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:])
}

// NewMCPKey son los datos de creación.
type NewMCPKey struct {
	Name      string
	FlowIDs   []string
	CreatedBy string
	ExpiresAt *time.Time
}

// CreateMCPKey inserta la credencial y devuelve la fila junto con la cadena
// completa, que es la única vez que existe.
//
// Los flujos del alcance se comprueban contra la organización dentro de la misma
// transacción: una key no puede nacer apuntando a un flujo de otro tenant, ni
// siquiera si quien la crea manda un id válido de otra parte.
func CreateMCPKey(ctx context.Context, p *pgxpool.Pool, orgID string, in NewMCPKey) (*MCPKey, string, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, "", fmt.Errorf("el nombre es obligatorio")
	}
	scope := normalizeFlowScope(in.FlowIDs)
	key, prefix, hash, err := NewMCPKeySecret()
	if err != nil {
		return nil, "", err
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if len(scope) > 0 {
		var reachable int
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM flows f JOIN bots b ON b.id = f.bot_id
			  WHERE b.org_id = $1::uuid AND f.id = ANY($2::uuid[])`,
			orgID, scope).Scan(&reachable); err != nil {
			return nil, "", err
		}
		if reachable != len(scope) {
			return nil, "", ErrMCPKeyInvalidScope
		}
	}

	rows, err := tx.Query(ctx,
		`INSERT INTO mcp_keys (org_id, name, key_prefix, key_hash, flow_ids, created_by, expires_at)
		 VALUES ($1::uuid, $2, $3, $4, $5::uuid[], NULLIF($6,''), $7)
		 RETURNING `+mcpKeyCols,
		orgID, name, prefix, hash, scope, in.CreatedBy, in.ExpiresAt)
	if err != nil {
		return nil, "", err
	}
	created, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[MCPKey])
	if err != nil {
		return nil, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	return &created, key, nil
}

// normalizeFlowScope descarta vacíos y repetidos. Un id vacío colado en la lista
// es la diferencia entre «acotada» y «toda la organización» escrita por
// accidente, así que no puede sobrevivir hasta la fila.
func normalizeFlowScope(flowIDs []string) []string {
	seen := make(map[string]bool, len(flowIDs))
	scope := make([]string, 0, len(flowIDs))
	for _, flowID := range flowIDs {
		flowID = strings.TrimSpace(flowID)
		if flowID == "" || seen[flowID] {
			continue
		}
		seen[flowID] = true
		scope = append(scope, flowID)
	}
	return scope
}

// ListMCPKeys devuelve las keys de una organización, vivas y revocadas. Las
// revocadas se conservan y se muestran: son el registro de qué se repartió.
func ListMCPKeys(ctx context.Context, p *pgxpool.Pool, orgID string) ([]MCPKey, error) {
	rows, err := p.Query(ctx,
		`SELECT `+mcpKeyCols+` FROM mcp_keys WHERE org_id = $1::uuid
		 ORDER BY revoked_at NULLS FIRST, created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[MCPKey])
}

// RevokeMCPKey marca la key como revocada. Devuelve nil si no existe o es de
// otra organización; el filtro por org_id es el aislamiento, no una comodidad.
//
// Revocar dos veces no es un error: conserva la primera marca y devuelve la
// fila, para que reintentar una revocación cuyo response se perdió no parezca
// que la key volvió a estar viva.
func RevokeMCPKey(ctx context.Context, p *pgxpool.Pool, orgID, keyID, revokedBy string) (*MCPKey, error) {
	rows, err := p.Query(ctx,
		`UPDATE mcp_keys
		    SET revoked_at = COALESCE(revoked_at, NOW()),
		        revoked_by = COALESCE(revoked_by, NULLIF($3,''))
		  WHERE id = $1::uuid AND org_id = $2::uuid
		 RETURNING `+mcpKeyCols, keyID, orgID, revokedBy)
	if err != nil {
		return nil, err
	}
	revoked, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[MCPKey])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &revoked, nil
}

// ResolveMCPKey traduce la cadena que llega en la cabecera a su fila. Devuelve
// nil cuando la key no existe, está revocada o caducó — los tres casos se
// funden a propósito: quien prueba credenciales no debe poder distinguir «no
// existe» de «existió y se la quitaron».
//
// Se consulta en **cada** petición y sin caché. Es lo que hace que revocar surta
// efecto en la llamada siguiente; una caché de un minuto sería un minuto de
// acceso después de haber cerrado la puerta.
func ResolveMCPKey(ctx context.Context, p *pgxpool.Pool, key string) (*MCPKey, error) {
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, MCPKeyPrefix) {
		return nil, nil
	}
	rows, err := p.Query(ctx,
		`SELECT `+mcpKeyCols+` FROM mcp_keys
		  WHERE key_hash = $1 AND revoked_at IS NULL
		    AND (expires_at IS NULL OR expires_at > NOW())`, HashMCPKey(key))
	if err != nil {
		return nil, err
	}
	resolved, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[MCPKey])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &resolved, nil
}

// TouchMCPKey refresca `last_used_at` solo si lleva rato sin tocarse. La
// condición va dentro del UPDATE y no en Go para que dos peticiones simultáneas
// no se pisen: la que llegue segunda no encontrará fila que actualizar.
func TouchMCPKey(ctx context.Context, p *pgxpool.Pool, keyID string) error {
	_, err := p.Exec(ctx,
		`UPDATE mcp_keys SET last_used_at = NOW()
		  WHERE id = $1::uuid
		    AND (last_used_at IS NULL OR last_used_at < NOW() - $2::interval)`,
		keyID, mcpKeyTouchInterval.String())
	return err
}
