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

	"github.com/Yzx7/sacs-chatbots/engine"
)

// Multiflujo (§3.1, §5.1 y §5.2 del plan). Un bot puede tener varios flujos: su
// flujo `message` de atención y uno o varios `schedule` de recordatorios.
// El borrador es editable; la versión publicada es inmutable y es la que ejecuta
// cada run, de modo que publicar no altera lo que ya está corriendo.

// Flow es un flujo del bot con su borrador y su versión publicada.
type Flow struct {
	ID          string `db:"id" json:"id"`
	BotID       string `db:"bot_id" json:"botId"`
	Key         string `db:"key" json:"key"`
	Name        string `db:"name" json:"name"`
	TriggerType string `db:"trigger_type" json:"triggerType"`
	Status      string `db:"status" json:"status"`
	Priority    int    `db:"priority" json:"priority"`
	IsFallback  bool   `db:"is_fallback" json:"isFallback"`
	// Audience restringe a quién atiende el flujo. NULL/vacío = atiende a todos.
	// Es la configuración de un `data_query`, no un lenguaje de filtros propio.
	Audience           json.RawMessage `db:"audience" json:"audience,omitempty"`
	Draft              json.RawMessage `db:"draft" json:"-"`
	PublishedVersionID *string         `db:"published_version_id" json:"publishedVersionId,omitempty"`
	// PublishedVersion es el número legible de la versión que ejecuta el bot.
	// Se hidrata junto con el checksum publicado, sin una consulta adicional.
	PublishedVersion *int       `db:"-" json:"publishedVersion,omitempty"`
	CreatedBy        *string    `db:"created_by" json:"createdBy,omitempty"`
	UpdatedBy        *string    `db:"updated_by" json:"updatedBy,omitempty"`
	ArchivedAt       *time.Time `db:"archived_at" json:"archivedAt,omitempty"`
	LastTickAt       *time.Time `db:"last_tick_at" json:"lastTickAt,omitempty"`
	CreatedAt        time.Time  `db:"created_at" json:"createdAt"`
	UpdatedAt        time.Time  `db:"updated_at" json:"updatedAt"`

	// UnpublishedChanges: el borrador no coincide con la versión que el bot
	// ejecuta. No sale de la base —se calcula comparando checksums— y por eso
	// lleva `db:"-"`.
	//
	// Existe porque el editor no tenía forma de decirlo: mostraba el estado del
	// flujo ("Publicado") junto a un lienzo que **siempre** edita el borrador, así
	// que un flujo publicado con cambios pendientes se veía exactamente igual que
	// uno al día. Se puede editar, guardar y creer que el bot ya lo usa.
	UnpublishedChanges bool `db:"-" json:"unpublishedChanges"`
	// CopilotCapability se hidrata en el borde HTTP. No proviene de PostgreSQL y
	// se omite en listados que no calculan capacidad/cuota para cada flujo.
	CopilotCapability *FlowCopilotCapability `db:"-" json:"copilotCapability,omitempty"`
}

// DraftSnapshot es el contrato versionado del borrador. Checksum es la revisión
// autoritativa para CAS; UpdatedAt/UpdatedBy son información de auditoría y no se
// usan como token de concurrencia.
type DraftSnapshot struct {
	Draft     json.RawMessage `json:"draft"`
	Checksum  string          `json:"checksum"`
	UpdatedAt time.Time       `json:"updatedAt"`
	UpdatedBy *string         `json:"updatedBy,omitempty"`
}

// DraftConflictError conserva el snapshot que ganó la carrera. Los controllers
// lo proyectan dentro de GenRes.data para que el editor no tenga que interpretar
// textos de error.
type DraftConflictError struct {
	Code             string          `json:"code"`
	ExpectedChecksum string          `json:"expectedChecksum"`
	CurrentChecksum  string          `json:"currentChecksum"`
	CurrentDraft     json.RawMessage `json:"currentDraft"`
	CurrentUpdatedAt time.Time       `json:"currentUpdatedAt"`
	CurrentUpdatedBy *string         `json:"currentUpdatedBy,omitempty"`
}

func (e *DraftConflictError) Error() string { return "el borrador cambió en otra sesión" }

// FlowValidationError distingue un grafo no publicable de un fallo de
// infraestructura. El borrador puede guardarse incompleto; solo PublishFlow lo
// produce.
type FlowValidationError struct{ Problem string }

func (e *FlowValidationError) Error() string { return e.Problem }

// NewDraftSnapshot normaliza el documento genérico, sin pasarlo por engine.Flow,
// para conservar pos y extensiones futuras del editor.
func NewDraftSnapshot(draft json.RawMessage, updatedAt time.Time, updatedBy *string) (*DraftSnapshot, error) {
	canonical, checksum, err := engine.CanonicalChecksum(draft)
	if err != nil {
		return nil, err
	}
	return &DraftSnapshot{
		Draft: canonical, Checksum: checksum, UpdatedAt: updatedAt, UpdatedBy: updatedBy,
	}, nil
}

// DraftSnapshotFromFlow proyecta los bytes y metadatos actuales de un Flow.
func DraftSnapshotFromFlow(flow *Flow) (*DraftSnapshot, error) {
	if flow == nil {
		return nil, nil
	}
	return NewDraftSnapshot(flow.Draft, flow.UpdatedAt, flow.UpdatedBy)
}

// MarshalJSON evita mantener dos contratos de lectura para el mismo borrador:
// cualquier Flow expuesto por HTTP lleva draftSnapshot y nunca el draft desnudo.
func (f Flow) MarshalJSON() ([]byte, error) {
	snapshot, err := DraftSnapshotFromFlow(&f)
	if err != nil {
		return nil, err
	}
	type flowAlias Flow
	return json.Marshal(struct {
		flowAlias
		DraftSnapshot *DraftSnapshot `json:"draftSnapshot"`
	}{
		flowAlias:     flowAlias(f),
		DraftSnapshot: snapshot,
	})
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
	// ErrFlowPublishOpenAudience cierra el TOCTOU entre la comprobación de rol
	// del controller y la audiencia de la fila bloqueada al publicar.
	ErrFlowPublishOpenAudience = errors.New("publicar un flujo sin audiencia requiere un administrador")
	// ErrFlowInvalidKey: la clave no cumple el formato.
	ErrFlowInvalidKey = errors.New("clave de flujo inválida (usa minúsculas, dígitos, - y _)")
)

const flowCols = `id::text AS id, bot_id::text AS bot_id, key, name, trigger_type, status,
	priority, is_fallback, audience, draft, published_version_id::text AS published_version_id,
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
	flows, err := pgx.CollectRows(rows, pgx.RowToStructByName[Flow])
	if err != nil {
		return nil, err
	}
	annotateUnpublished(ctx, p, flows)
	return flows, nil
}

// annotateUnpublished marca los flujos cuyo borrador difiere de lo publicado.
//
// Compara por **checksum canónico**, la misma normalización que usa `PublishFlow`
// para detectar un publicado sin cambios. Así el editor y el publicador coinciden
// en qué significa "es lo mismo": si aquí dijera que hay cambios y publicar los
// declarara no-op, el aviso se volvería ruido y se aprendería a ignorarlo.
//
// Un flujo sin versión publicada tiene cambios por definición: nada de lo que
// está escrito se está ejecutando.
func annotateUnpublished(ctx context.Context, p *pgxpool.Pool, flows []Flow) {
	ids := make([]string, 0, len(flows))
	for i := range flows {
		if flows[i].PublishedVersionID != nil {
			ids = append(ids, *flows[i].PublishedVersionID)
		} else {
			flows[i].UnpublishedChanges = len(flows[i].Draft) > 0
		}
	}
	if len(ids) == 0 {
		return
	}
	rows, err := p.Query(ctx, `SELECT id::text, checksum, version FROM flow_versions WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return
	}
	defer rows.Close()
	type publishedFlowVersion struct {
		checksum string
		version  int
	}
	publishedVersions := make(map[string]publishedFlowVersion, len(ids))
	for rows.Next() {
		var id, checksum string
		var version int
		if rows.Scan(&id, &checksum, &version) == nil {
			publishedVersions[id] = publishedFlowVersion{checksum: checksum, version: version}
		}
	}
	for i := range flows {
		if flows[i].PublishedVersionID == nil {
			continue
		}
		published, ok := publishedVersions[*flows[i].PublishedVersionID]
		if !ok {
			continue
		}
		version := published.version
		flows[i].PublishedVersion = &version
		_, draft, err := engine.CanonicalChecksum(flows[i].Draft)
		if err != nil {
			// Un borrador que ni siquiera normaliza no es lo publicado.
			flows[i].UnpublishedChanges = true
			continue
		}
		flows[i].UnpublishedChanges = draft != published.checksum
	}
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
	single := []Flow{f}
	annotateUnpublished(ctx, p, single)
	return &single[0], nil
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

// UpdateFlowDraft guarda el borrador mediante compare-and-swap. No toca la
// versión publicada: editar no cambia lo que se está ejecutando (§3.4).
//
// El no-op por contenido se comprueba antes que expectedChecksum para que
// reintentar exactamente una escritura cuyo response se perdió sea idempotente.
func UpdateFlowDraft(
	ctx context.Context,
	p *pgxpool.Pool,
	botID, flowID string,
	draft json.RawMessage,
	expectedChecksum, userID string,
) (*DraftSnapshot, error) {
	canonical, _, err := engine.CanonicalChecksum(draft)
	if err != nil {
		return nil, err
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	flow, err := lockFlow(ctx, tx, botID, flowID)
	if err != nil || flow == nil {
		return nil, err
	}
	before, err := DraftSnapshotFromFlow(flow)
	if err != nil {
		return nil, err
	}
	snapshot, err := updateFlowDraftTx(ctx, tx, flow, canonical, expectedChecksum, userID)
	if err != nil {
		return nil, err
	}
	if snapshot.Checksum != before.Checksum {
		if err := reconcileFlowCopilotProposalsAfterDraftUpdateTx(ctx, tx, flow.ID,
			snapshot.Draft, snapshot.Checksum, ""); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func lockFlow(ctx context.Context, tx pgx.Tx, botID, flowID string) (*Flow, error) {
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
	return &flow, nil
}

// updateFlowDraftTx exige que flow provenga de lockFlow dentro de tx. Se deja
// separado para que restauración y, después, propuestas/undo compartan la misma
// primitiva sin abrir transacciones anidadas.
func updateFlowDraftTx(
	ctx context.Context,
	tx pgx.Tx,
	flow *Flow,
	draft json.RawMessage,
	expectedChecksum, userID string,
) (*DraftSnapshot, error) {
	if flow.ArchivedAt != nil {
		return nil, ErrFlowArchived
	}
	current, err := DraftSnapshotFromFlow(flow)
	if err != nil {
		return nil, err
	}
	// Antes de canonizar: el motor no lee edges[].id, pero React Flow lo exige
	// para dibujar. Sin esto, un grafo escrito por MCP o por el Copilot valida,
	// corre bien y aparece en el editor sin ninguna conexión. Se rellena aquí
	// —el único punto por el que pasan todas las escrituras del borrador— y no
	// en cada llamador.
	draft, err = engine.NormalizeEdgeIDs(draft)
	if err != nil {
		return nil, err
	}
	canonical, candidateChecksum, err := engine.CanonicalChecksum(draft)
	if err != nil {
		return nil, err
	}
	if candidateChecksum == current.Checksum {
		return current, nil
	}
	if expectedChecksum != current.Checksum {
		return nil, newDraftConflict(expectedChecksum, current)
	}

	rows, err := tx.Query(ctx,
		`UPDATE flows SET draft = $2, updated_by = COALESCE(NULLIF($3,''), updated_by)
		 WHERE id = $1::uuid AND archived_at IS NULL
		 RETURNING `+flowCols, flow.ID, canonical, userID)
	if err != nil {
		return nil, err
	}
	updated, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Flow])
	if err != nil {
		return nil, err
	}
	return DraftSnapshotFromFlow(&updated)
}

func newDraftConflict(expectedChecksum string, current *DraftSnapshot) *DraftConflictError {
	return &DraftConflictError{
		Code:             "draft_conflict",
		ExpectedChecksum: expectedChecksum,
		CurrentChecksum:  current.Checksum,
		CurrentDraft:     current.Draft,
		CurrentUpdatedAt: current.UpdatedAt,
		CurrentUpdatedBy: current.UpdatedBy,
	}
}

// UpdateFlowMeta renombra o cambia prioridad/fallback sin tocar el grafo.
//
// Marcar el fallback **desmarca al anterior en la misma transacción**. El índice
// uq_flows_bot_fallback solo admite uno vivo por bot, así que sin ese paso previo
// elegir otro fallback obligaría a desmarcar a mano el actual: dos operaciones
// para una sola decisión, con el bot sin red de seguridad en medio.
func UpdateFlowMeta(ctx context.Context, p *pgxpool.Pool, botID, flowID, name string, priority int, isFallback bool, userID string) (*Flow, error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if isFallback {
		if _, err := tx.Exec(ctx,
			`UPDATE flows SET is_fallback = FALSE, updated_by = COALESCE(NULLIF($3,''), updated_by)
			 WHERE bot_id = $1::uuid AND id <> $2::uuid AND is_fallback
			   AND archived_at IS NULL AND trigger_type = 'message'`,
			botID, flowID, userID); err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query(ctx,
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
	if err := tx.Commit(ctx); err != nil {
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

// PublishFlow crea (o reutiliza) la versión publicada del borrador que está
// realmente bloqueado en PostgreSQL. El caller solo aporta la revisión que la
// persona revisó; nunca aporta definition/checksum como fuente publicable.
//
// Canonicalización, validación del motor, bindings de plantillas, creación de la
// versión y actualización de published_version_id ocurren dentro de esta única
// transacción. Dos publish simultáneos no pueden crear dos veces la versión N ni
// validar un snapshot y terminar publicando otro.
func PublishFlow(
	ctx context.Context,
	p *pgxpool.Pool,
	botID, flowID, expectedDraftChecksum, userID string,
	canPublishOpen bool,
) (*PublishResult, error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	flow, err := lockFlow(ctx, tx, botID, flowID)
	if err != nil || flow == nil {
		return nil, err
	}
	if flow.ArchivedAt != nil {
		return nil, ErrFlowArchived
	}
	currentDraft, err := DraftSnapshotFromFlow(flow)
	if err != nil {
		return nil, err
	}
	if expectedDraftChecksum != currentDraft.Checksum {
		return nil, newDraftConflict(expectedDraftChecksum, currentDraft)
	}
	if len(flow.Audience) == 0 && !canPublishOpen {
		return nil, ErrFlowPublishOpenAudience
	}

	definition, checksum, templateWarnings, err := validateLockedFlowForPublish(ctx, tx, botID, flow)
	if err != nil {
		return nil, err
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
			currentVersion := current.Version
			updated.PublishedVersion = &currentVersion
			return &PublishResult{
				Flow: &updated, Version: &current, Created: false, Warnings: templateWarnings,
			}, nil
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
	publishedVersion := version.Version
	updated.PublishedVersion = &publishedVersion
	return &PublishResult{
		Flow: &updated, Version: &version, Created: true, Warnings: templateWarnings,
	}, nil
}

func validateLockedFlowForPublish(
	ctx context.Context,
	tx pgx.Tx,
	botID string,
	flow *Flow,
) ([]byte, string, []string, error) {
	definition, checksum, err := engine.CanonicalChecksum(flow.Draft)
	if err != nil {
		return nil, "", nil, &FlowValidationError{Problem: err.Error()}
	}
	var parsed engine.Flow
	if err := json.Unmarshal(definition, &parsed); err != nil {
		return nil, "", nil, &FlowValidationError{Problem: "flujo inválido (no es JSON del editor)"}
	}
	if parsed.Trigger.Type != flow.TriggerType {
		return nil, "", nil, &FlowValidationError{Problem: "el trigger del grafo (" + parsed.Trigger.Type +
			") no coincide con el del flujo (" + flow.TriggerType + ")"}
	}
	if err := engine.Validate(&parsed); err != nil {
		return nil, "", nil, &FlowValidationError{Problem: err.Error()}
	}
	templateValidation, err := validateFlowTemplatesWithQuery(ctx, tx, botID, definition)
	if err != nil {
		return nil, "", nil, &FlowValidationError{Problem: err.Error()}
	}
	return definition, checksum, templateValidation.Warnings, nil
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
func RestoreFlowVersion(
	ctx context.Context,
	p *pgxpool.Pool,
	botID, flowID, versionID, expectedDraftChecksum, userID string,
) (*DraftSnapshot, error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	flow, err := lockFlow(ctx, tx, botID, flowID)
	if err != nil || flow == nil {
		return nil, err
	}
	if flow.ArchivedAt != nil {
		return nil, ErrFlowArchived
	}
	rows, err := tx.Query(ctx,
		`SELECT `+flowVersionCols+` FROM flow_versions WHERE id = $1::uuid AND flow_id = $2::uuid`,
		versionID, flow.ID)
	if err != nil {
		return nil, err
	}
	version, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowVersion])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	snapshot, err := updateFlowDraftTx(ctx, tx, flow, version.Definition, expectedDraftChecksum, userID)
	if err != nil {
		return nil, err
	}
	if err := reconcileFlowCopilotProposalsAfterDraftUpdateTx(ctx, tx, flow.ID,
		snapshot.Draft, snapshot.Checksum, ""); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// PublishedFlowRef es un flujo publicado junto a su grafo vigente. Lo consume el
// dispatcher del webhook, que necesita saber **cuál** flujo eligió (para poder
// reanudar la conversación en ese mismo grafo), no solo su definición.
type PublishedFlowRef struct {
	FlowID     string `db:"flow_id"`
	Key        string `db:"key"`
	Name       string `db:"name"`
	Priority   int    `db:"priority"`
	IsFallback bool   `db:"is_fallback"`
	// Audience viaja con el flujo publicado aunque viva en la fila mutable: el
	// dispatcher la necesita en el mismo lote para no consultar `flows` otra vez
	// dentro del bucle.
	Audience   json.RawMessage `db:"audience"`
	Definition json.RawMessage `db:"definition"`
}

// PublishedMessageFlows devuelve todos los flujos `message` publicados del bot,
// en el orden en que el dispatcher debe probarlos: primero los que declaran
// palabras clave (por prioridad), y el fallback al final.
//
// El orden por `is_fallback` no es cosmético: el fallback es el que atiende
// cuando ninguno reconoce el mensaje, así que probarlo antes lo convertiría en el
// único flujo que se ejecuta jamás.
func PublishedMessageFlows(ctx context.Context, p *pgxpool.Pool, botID string) ([]PublishedFlowRef, error) {
	rows, err := p.Query(ctx,
		`SELECT f.id::text AS flow_id, f.key, f.name, f.priority, f.is_fallback, f.audience, v.definition
		 FROM flows f
		 JOIN flow_versions v ON v.id = f.published_version_id
		 WHERE f.bot_id = $1::uuid AND f.trigger_type = 'message'
		   AND f.status = 'published' AND f.archived_at IS NULL
		 ORDER BY f.is_fallback, f.priority, f.id`, botID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[PublishedFlowRef])
}

// PublishedFlowByID devuelve el grafo publicado de un flujo concreto (nil si no
// existe, no es del bot, está archivado, pausado o sin versión publicada).
//
// Reanudar exige esto: una conversación a medias tiene que seguir en el grafo en
// el que empezó aunque el mensaje siguiente ya no case con su trigger. Si el
// flujo dejó de estar publicado, devolver nil es lo correcto — el llamador vuelve
// a despachar en vez de ejecutar algo que el operador pausó.
func PublishedFlowByID(ctx context.Context, p *pgxpool.Pool, botID, flowID string) (*PublishedFlowRef, error) {
	rows, err := p.Query(ctx,
		`SELECT f.id::text AS flow_id, f.key, f.name, f.priority, f.is_fallback, f.audience, v.definition
		 FROM flows f
		 JOIN flow_versions v ON v.id = f.published_version_id
		 WHERE f.bot_id = $1::uuid AND f.id = $2::uuid
		   AND f.status = 'published' AND f.archived_at IS NULL`, botID, flowID)
	if err != nil {
		return nil, err
	}
	ref, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[PublishedFlowRef])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ref, nil
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
