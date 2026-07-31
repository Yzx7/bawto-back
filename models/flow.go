package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Multiflujo (§3.1, §5.1 y §5.2 del plan). Un bot puede tener varios flujos: su
// flujo `message` de atención y uno o varios `schedule` de recordatorios.
// El borrador es editable; la versión publicada es inmutable y es la que ejecuta
// cada run, de modo que publicar no altera lo que ya está corriendo.

// Flow es un flujo del bot con su borrador y su versión publicada.
type Flow struct {
	ID                 string          `db:"id" json:"id"`
	BotID              string          `db:"bot_id" json:"botId"`
	Key                string          `db:"key" json:"key"`
	Name               string          `db:"name" json:"name"`
	TriggerType        string          `db:"trigger_type" json:"triggerType"`
	Status             string          `db:"status" json:"status"`
	Priority           int             `db:"priority" json:"priority"`
	IsFallback         bool            `db:"is_fallback" json:"isFallback"`
	Draft              json.RawMessage `db:"draft" json:"draft"`
	PublishedVersionID *string         `db:"published_version_id" json:"publishedVersionId,omitempty"`
	CreatedBy          *string         `db:"created_by" json:"createdBy,omitempty"`
	UpdatedBy          *string         `db:"updated_by" json:"updatedBy,omitempty"`
	ArchivedAt         *time.Time      `db:"archived_at" json:"archivedAt,omitempty"`
	LastTickAt         *time.Time      `db:"last_tick_at" json:"lastTickAt,omitempty"`
	CreatedAt          time.Time       `db:"created_at" json:"createdAt"`
	UpdatedAt          time.Time       `db:"updated_at" json:"updatedAt"`
}

// FlowVersion es una definición publicada e inmutable.
type FlowVersion struct {
	ID          string          `db:"id" json:"id"`
	FlowID      string          `db:"flow_id" json:"flowId"`
	Version     int             `db:"version" json:"version"`
	Definition  json.RawMessage `db:"definition" json:"definition"`
	Checksum    string          `db:"checksum" json:"checksum"`
	PublishedBy *string         `db:"published_by" json:"publishedBy,omitempty"`
	PublishedAt time.Time       `db:"published_at" json:"publishedAt"`
}

var (
	// ErrFlowKeyTaken: otro flujo vivo del mismo bot ya usa esa key. Archivar el
	// anterior la libera (índice único parcial uq_flows_bot_key).
	ErrFlowKeyTaken = errors.New("ya existe un flujo con esa clave en el bot")
	// ErrFlowFallbackTaken: solo un flujo `message` vivo puede ser el fallback.
	ErrFlowFallbackTaken = errors.New("el bot ya tiene un flujo message marcado como fallback")
	// ErrFlowArchived: un flujo archivado no se edita ni se publica.
	ErrFlowArchived = errors.New("el flujo está archivado")
	// ErrFlowInvalidKey: la clave no cumple el formato.
	ErrFlowInvalidKey = errors.New("clave de flujo inválida (usa minúsculas, dígitos, - y _)")
)

const flowCols = `id::text AS id, bot_id::text AS bot_id, key, name, trigger_type, status,
	priority, is_fallback, draft, published_version_id::text AS published_version_id,
	created_by, updated_by, archived_at, last_tick_at, created_at, updated_at`

// Sin calificar por tabla: no se puede usar en un SELECT con JOIN a `flows`,
// porque `id` y `flow_id` quedarían ambiguos. Si hace falta filtrar por el bot
// dueño, usa una subconsulta sobre `flows` en vez de un JOIN.
const flowVersionCols = `id::text AS id, flow_id::text AS flow_id, version, definition,
	checksum, published_by, published_at`

var flowKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// ValidFlowKey replica el CHECK de la tabla para poder responder 400 en vez de 500.
func ValidFlowKey(key string) bool { return flowKeyRe.MatchString(key) }

// FlowKeyFromDefinition deriva una key estable a partir del grafo. La usa el
// backfill de `bots.flow` (§12 pasos 3-4) para que re-ejecutarlo no duplique.
func FlowKeyFromDefinition(flowID, fallback string) string {
	key := slugFlowKey(flowID)
	if key == "" {
		key = slugFlowKey(fallback)
	}
	if key == "" {
		key = "principal"
	}
	return key
}

func slugFlowKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_':
			b.WriteRune('_')
		case r == '-' || r == ' ':
			// Se colapsan los separadores: "WAA — Atención" no debe producir
			// "waa---atencin".
			if s := b.String(); s != "" && !strings.HasSuffix(s, "-") {
				b.WriteRune('-')
			}
		}
	}
	key := strings.Trim(b.String(), "-_")
	for len(key) > 0 && (key[0] < 'a' || key[0] > 'z') {
		key = key[1:]
	}
	if len(key) > 63 {
		key = strings.Trim(key[:63], "-_")
	}
	if !ValidFlowKey(key) {
		return ""
	}
	return key
}

// ListFlows devuelve los flujos del bot. Los archivados solo si se piden: la
// lista operativa (§11.1) no debe mostrarlos.
func ListFlows(ctx context.Context, p *pgxpool.Pool, botID string, includeArchived bool) ([]Flow, error) {
	rows, err := p.Query(ctx, `SELECT `+flowCols+` FROM flows
		WHERE bot_id = $1::uuid AND ($2 OR archived_at IS NULL)
		ORDER BY trigger_type, priority, created_at`, botID, includeArchived)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Flow])
}

// GetFlow devuelve un flujo del bot (nil si no existe o es de otro bot). El
// filtro por bot_id es el aislamiento multi-tenant: la autorización de la org se
// resuelve antes, sobre el bot (botWithRole).
func GetFlow(ctx context.Context, p *pgxpool.Pool, botID, flowID string) (*Flow, error) {
	rows, err := p.Query(ctx,
		`SELECT `+flowCols+` FROM flows WHERE id = $1::uuid AND bot_id = $2::uuid`, flowID, botID)
	if err != nil {
		return nil, err
	}
	f, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Flow])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// NewFlow son los datos de creación de un flujo.
type NewFlow struct {
	Key         string
	Name        string
	TriggerType string
	Priority    int
	IsFallback  bool
	Draft       json.RawMessage
	UserID      string
}

// CreateFlow crea un flujo en estado draft.
func CreateFlow(ctx context.Context, p *pgxpool.Pool, botID string, in NewFlow) (*Flow, error) {
	if !ValidFlowKey(in.Key) {
		return nil, ErrFlowInvalidKey
	}
	if len(in.Draft) == 0 {
		in.Draft = json.RawMessage(`{}`)
	}
	if in.Priority == 0 {
		in.Priority = 100
	}
	rows, err := p.Query(ctx,
		`INSERT INTO flows (bot_id, key, name, trigger_type, priority, is_fallback, draft, created_by, updated_by)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, NULLIF($8,''), NULLIF($8,''))
		 RETURNING `+flowCols,
		botID, in.Key, in.Name, in.TriggerType, in.Priority, in.IsFallback, in.Draft, in.UserID)
	if err != nil {
		return nil, translateFlowConflict(err)
	}
	f, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Flow])
	if err != nil {
		return nil, translateFlowConflict(err)
	}
	return &f, nil
}

// UpdateFlowDraft guarda el borrador. No toca la versión publicada: editar no
// cambia lo que se está ejecutando (§3.4).
func UpdateFlowDraft(ctx context.Context, p *pgxpool.Pool, botID, flowID string, draft json.RawMessage, userID string) (*Flow, error) {
	rows, err := p.Query(ctx,
		`UPDATE flows SET draft = $3, updated_by = COALESCE(NULLIF($4,''), updated_by)
		 WHERE id = $1::uuid AND bot_id = $2::uuid AND archived_at IS NULL
		 RETURNING `+flowCols, flowID, botID, draft, userID)
	if err != nil {
		return nil, err
	}
	f, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Flow])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// UpdateFlowMeta renombra o cambia prioridad/fallback sin tocar el grafo.
func UpdateFlowMeta(ctx context.Context, p *pgxpool.Pool, botID, flowID, name string, priority int, isFallback bool, userID string) (*Flow, error) {
	rows, err := p.Query(ctx,
		`UPDATE flows SET name = $3, priority = $4, is_fallback = $5,
		        updated_by = COALESCE(NULLIF($6,''), updated_by)
		 WHERE id = $1::uuid AND bot_id = $2::uuid AND archived_at IS NULL
		 RETURNING `+flowCols, flowID, botID, name, priority, isFallback, userID)
	if err != nil {
		return nil, translateFlowConflict(err)
	}
	f, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Flow])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, translateFlowConflict(err)
	}
	return &f, nil
}

// PauseFlow deja de crear ejecuciones nuevas sin perder la versión publicada.
// Solo tiene sentido sobre un flujo publicado o ya pausado.
func PauseFlow(ctx context.Context, p *pgxpool.Pool, botID, flowID, userID string) (*Flow, error) {
	return setFlowStatus(ctx, p, botID, flowID, "paused", userID)
}

// ResumeFlow devuelve a `published` un flujo pausado que conserva versión.
func ResumeFlow(ctx context.Context, p *pgxpool.Pool, botID, flowID, userID string) (*Flow, error) {
	rows, err := p.Query(ctx,
		`UPDATE flows SET status = 'published', updated_by = COALESCE(NULLIF($3,''), updated_by),
		        last_tick_at = CASE WHEN trigger_type='schedule' THEN NOW() ELSE last_tick_at END
		 WHERE id = $1::uuid AND bot_id = $2::uuid AND archived_at IS NULL
		   AND status = 'paused' AND published_version_id IS NOT NULL
		 RETURNING `+flowCols, flowID, botID, userID)
	if err != nil {
		return nil, err
	}
	f, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Flow])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func setFlowStatus(ctx context.Context, p *pgxpool.Pool, botID, flowID, status, userID string) (*Flow, error) {
	rows, err := p.Query(ctx,
		`UPDATE flows SET status = $3, updated_by = COALESCE(NULLIF($4,''), updated_by)
		 WHERE id = $1::uuid AND bot_id = $2::uuid AND archived_at IS NULL
		 RETURNING `+flowCols, flowID, botID, status, userID)
	if err != nil {
		return nil, err
	}
	f, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Flow])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// ArchiveFlow archiva el flujo y **libera su key**: duplicar un recordatorio
// archivado no debe obligar a inventar "recordatorio-d3-v2" (§5.1).
func ArchiveFlow(ctx context.Context, p *pgxpool.Pool, botID, flowID, userID string) (*Flow, error) {
	rows, err := p.Query(ctx,
		`UPDATE flows SET status = 'archived', archived_at = NOW(),
		        updated_by = COALESCE(NULLIF($3,''), updated_by)
		 WHERE id = $1::uuid AND bot_id = $2::uuid AND archived_at IS NULL
		 RETURNING `+flowCols, flowID, botID, userID)
	if err != nil {
		return nil, err
	}
	f, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Flow])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// DuplicateFlow copia un flujo como borrador nuevo.
//
// Copia el grafo **publicado** cuando existe, no el borrador: lo que el operador
// quiere clonar es lo que está corriendo, no un experimento a medio guardar. La
// copia nace en `draft` y nunca hereda `is_fallback`, porque solo un flujo
// `message` vivo puede serlo (§5.1) y duplicar no es una forma de reemplazarlo.
func DuplicateFlow(ctx context.Context, p *pgxpool.Pool, botID, flowID, userID string) (*Flow, error) {
	source, err := GetFlow(ctx, p, botID, flowID)
	if err != nil || source == nil {
		return nil, err
	}
	definition := source.Draft
	if source.PublishedVersionID != nil {
		var published json.RawMessage
		err := p.QueryRow(ctx, `SELECT definition FROM flow_versions WHERE id = $1::uuid`,
			*source.PublishedVersionID).Scan(&published)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if len(published) > 0 {
			definition = published
		}
	}
	key, err := nextCopyKey(ctx, p, botID, source.Key)
	if err != nil {
		return nil, err
	}
	return CreateFlow(ctx, p, botID, NewFlow{
		Key:         key,
		Name:        source.Name + " (copia)",
		TriggerType: source.TriggerType,
		Priority:    source.Priority,
		IsFallback:  false,
		Draft:       definition,
		UserID:      userID,
	})
}

// nextCopyKey busca la primera key libre de la forma `<base>-copia[-n]`. Solo
// compiten las keys de flujos vivos: archivar libera la suya (§5.1).
func nextCopyKey(ctx context.Context, p *pgxpool.Pool, botID, base string) (string, error) {
	// Se recorta antes de añadir el sufijo para no pasar de los 63 caracteres
	// que admite el CHECK de la tabla.
	if len(base) > 50 {
		base = strings.Trim(base[:50], "-_")
	}
	for i := 1; i <= 50; i++ {
		candidate := base + "-copia"
		if i > 1 {
			candidate = fmt.Sprintf("%s-copia-%d", base, i)
		}
		if !ValidFlowKey(candidate) {
			return "", ErrFlowInvalidKey
		}
		var taken bool
		err := p.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM flows
			WHERE bot_id = $1::uuid AND key = $2 AND archived_at IS NULL)`, botID, candidate).Scan(&taken)
		if err != nil {
			return "", err
		}
		if !taken {
			return candidate, nil
		}
	}
	return "", ErrFlowKeyTaken
}

// PublishResult es el resultado de publicar: la versión vigente y si se creó.
type PublishResult struct {
	Flow    *Flow        `json:"flow"`
	Version *FlowVersion `json:"version"`
	// Created=false significa no-op: el grafo es idéntico al ya publicado y se
	// devolvió la versión existente en vez de crear una nueva (§5.2, paso 4).
	Created  bool     `json:"created"`
	Warnings []string `json:"warnings,omitempty"`
}

// PublishFlow crea (o reutiliza) la versión publicada del borrador.
//
// `definition` debe venir ya validada por engine.Validate y normalizada por
// engine.Canonical; el checksum se calcula sobre esa forma normalizada.
//
// Toda la operación va en una transacción con el flujo bloqueado: dos publish
// simultáneos no pueden crear dos veces la versión N.
func PublishFlow(ctx context.Context, p *pgxpool.Pool, botID, flowID string, definition []byte, checksum, userID string) (*PublishResult, error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	rows, err := tx.Query(ctx,
		`SELECT `+flowCols+` FROM flows WHERE id = $1::uuid AND bot_id = $2::uuid FOR UPDATE`,
		flowID, botID)
	if err != nil {
		return nil, err
	}
	flow, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Flow])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if flow.ArchivedAt != nil {
		return nil, ErrFlowArchived
	}

	// No-op por checksum: republicar el mismo grafo no crea una versión nueva.
	if flow.PublishedVersionID != nil {
		vrows, err := tx.Query(ctx,
			`SELECT `+flowVersionCols+` FROM flow_versions WHERE id = $1::uuid`, *flow.PublishedVersionID)
		if err != nil {
			return nil, err
		}
		current, err := pgx.CollectExactlyOneRow(vrows, pgx.RowToStructByName[FlowVersion])
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if err == nil && current.Checksum == checksum {
			// Solo se asegura el estado: un flujo pausado que republica lo mismo
			// vuelve a estar publicado.
			urows, err := tx.Query(ctx,
				`UPDATE flows SET status='published', updated_by = COALESCE(NULLIF($2,''), updated_by),
				        last_tick_at = CASE WHEN trigger_type='schedule' THEN NOW() ELSE last_tick_at END
				 WHERE id = $1::uuid RETURNING `+flowCols, flow.ID, userID)
			if err != nil {
				return nil, err
			}
			updated, err := pgx.CollectExactlyOneRow(urows, pgx.RowToStructByName[Flow])
			if err != nil {
				return nil, err
			}
			if err := tx.Commit(ctx); err != nil {
				return nil, err
			}
			return &PublishResult{Flow: &updated, Version: &current, Created: false}, nil
		}
	}

	var next int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM flow_versions WHERE flow_id = $1::uuid`,
		flow.ID).Scan(&next); err != nil {
		return nil, err
	}

	nrows, err := tx.Query(ctx,
		`INSERT INTO flow_versions (flow_id, version, definition, checksum, published_by)
		 VALUES ($1::uuid, $2, $3, $4, NULLIF($5,''))
		 RETURNING `+flowVersionCols, flow.ID, next, definition, checksum, userID)
	if err != nil {
		return nil, err
	}
	version, err := pgx.CollectExactlyOneRow(nrows, pgx.RowToStructByName[FlowVersion])
	if err != nil {
		return nil, err
	}

	frows, err := tx.Query(ctx,
		`UPDATE flows SET published_version_id = $2::uuid, status = 'published',
		        updated_by = COALESCE(NULLIF($3,''), updated_by),
		        last_tick_at = CASE WHEN trigger_type='schedule' THEN NOW() ELSE last_tick_at END
		 WHERE id = $1::uuid RETURNING `+flowCols, flow.ID, version.ID, userID)
	if err != nil {
		return nil, err
	}
	updated, err := pgx.CollectExactlyOneRow(frows, pgx.RowToStructByName[Flow])
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &PublishResult{Flow: &updated, Version: &version, Created: true}, nil
}

// ListFlowVersions devuelve el historial, de la más reciente a la más vieja.
func ListFlowVersions(ctx context.Context, p *pgxpool.Pool, botID, flowID string) ([]FlowVersion, error) {
	rows, err := p.Query(ctx,
		`SELECT `+flowVersionCols+` FROM flow_versions v
		 WHERE v.flow_id = (SELECT id FROM flows WHERE id = $1::uuid AND bot_id = $2::uuid)
		 ORDER BY v.version DESC`, flowID, botID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[FlowVersion])
}

// GetFlowVersion devuelve una versión concreta del flujo (nil si no es suya).
func GetFlowVersion(ctx context.Context, p *pgxpool.Pool, botID, flowID, versionID string) (*FlowVersion, error) {
	rows, err := p.Query(ctx,
		`SELECT `+flowVersionCols+` FROM flow_versions
		 WHERE id = $1::uuid
		   AND flow_id = (SELECT id FROM flows WHERE id = $2::uuid AND bot_id = $3::uuid)`,
		versionID, flowID, botID)
	if err != nil {
		return nil, err
	}
	v, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowVersion])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// RestoreFlowVersion copia una versión al borrador. **No** publica: republicar
// queda como acto explícito, y por eso flow_versions no lleva UNIQUE(flow_id,
// checksum) — restaurar y publicar crea una versión nueva con el mismo checksum.
func RestoreFlowVersion(ctx context.Context, p *pgxpool.Pool, botID, flowID, versionID, userID string) (*Flow, error) {
	version, err := GetFlowVersion(ctx, p, botID, flowID, versionID)
	if err != nil || version == nil {
		return nil, err
	}
	return UpdateFlowDraft(ctx, p, botID, flowID, version.Definition, userID)
}

// PublishedFlowDefinition devuelve la definición publicada del flujo de ese tipo
// con mayor prioridad (menor `priority`). Es la lectura compatible de §12: el
// llamador cae a `bots.flow` si esto devuelve nil.
//
// Los flujos pausados quedan fuera a propósito: pausar debe dejar de ejecutar.
func PublishedFlowDefinition(ctx context.Context, p *pgxpool.Pool, botID, triggerType string) (json.RawMessage, error) {
	var def json.RawMessage
	err := p.QueryRow(ctx,
		`SELECT v.definition FROM flows f
		 JOIN flow_versions v ON v.id = f.published_version_id
		 WHERE f.bot_id = $1::uuid AND f.trigger_type = $2
		   AND f.status = 'published' AND f.archived_at IS NULL
		 ORDER BY f.is_fallback, f.priority, f.id
		 LIMIT 1`, botID, triggerType).Scan(&def)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return def, nil
}

// translateFlowConflict convierte los conflictos de índice en errores de dominio,
// para que el controlador responda 409/400 en vez de un 500 opaco.
func translateFlowConflict(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.ConstraintName {
	case "uq_flows_bot_key":
		return ErrFlowKeyTaken
	case "uq_flows_bot_fallback":
		return ErrFlowFallbackTaken
	case "flows_key_check":
		return ErrFlowInvalidKey
	}
	return err
}
