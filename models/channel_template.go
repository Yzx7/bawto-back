package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yzx7/sacs-chatbots/channels/whatsapp"
	"github.com/Yzx7/sacs-chatbots/engine"
)

type ChannelTemplate struct {
	ID                       string          `db:"id" json:"id"`
	WabaID                   string          `db:"waba_id" json:"wabaId"`
	MetaTemplateID           *string         `db:"meta_template_id" json:"metaTemplateId,omitempty"`
	Name                     string          `db:"name" json:"name"`
	Language                 string          `db:"language" json:"language"`
	Status                   string          `db:"status" json:"status"`
	Category                 *string         `db:"category" json:"category,omitempty"`
	QualityScore             *string         `db:"quality_score" json:"qualityScore,omitempty"`
	ParameterFormat          *string         `db:"parameter_format" json:"parameterFormat,omitempty"`
	Components               json.RawMessage `db:"components" json:"components"`
	ParameterSchema          json.RawMessage `db:"parameter_schema" json:"parameterSchema"`
	BodyParameterCount       int             `db:"body_parameter_count" json:"bodyParameterCount"`
	HasUnsupportedParameters bool            `db:"has_unsupported_parameters" json:"hasUnsupportedParameters"`
	RejectedReason           *string         `db:"rejected_reason" json:"rejectedReason,omitempty"`
	PendingCategory          *string         `db:"pending_category" json:"pendingCategory,omitempty"`
	CategoryChangeAt         *time.Time      `db:"category_change_at" json:"categoryChangeAt,omitempty"`
	MetaUpdatedAt            *time.Time      `db:"meta_updated_at" json:"metaUpdatedAt,omitempty"`
	LastEventAt              *time.Time      `db:"last_event_at" json:"lastEventAt,omitempty"`
	LastSyncedAt             *time.Time      `db:"last_synced_at" json:"lastSyncedAt,omitempty"`
	CreatedAt                time.Time       `db:"created_at" json:"createdAt"`
	UpdatedAt                time.Time       `db:"updated_at" json:"updatedAt"`
}

const channelTemplateCols = `t.id::text AS id,t.waba_id,t.meta_template_id,t.name,t.language,t.status,t.category,
	t.quality_score,t.parameter_format,t.components,t.parameter_schema,t.body_parameter_count,
	t.has_unsupported_parameters,t.rejected_reason,t.pending_category,t.category_change_at,
	t.meta_updated_at,t.last_event_at,t.last_synced_at,t.created_at,t.updated_at`

type TemplateSyncReport struct {
	Total         int       `json:"total"`
	MarkedDeleted int64     `json:"markedDeleted"`
	SyncedAt      time.Time `json:"syncedAt"`
}

func metaTimestamp(raw json.RawMessage) *time.Time {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return nil
	}
	if seconds, err := time.ParseDuration(strings.Trim(value, `"`) + "s"); err == nil {
		at := time.Unix(int64(seconds.Seconds()), 0).UTC()
		return &at
	}
	if parsed, err := time.Parse(time.RFC3339, strings.Trim(value, `"`)); err == nil {
		at := parsed.UTC()
		return &at
	}
	return nil
}

func normalizedComponents(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) || string(raw) == "null" {
		return json.RawMessage(`[]`)
	}
	return raw
}

// SyncChannelTemplates reemplaza atómicamente la fotografía del WABA. Las
// plantillas que desaparecen de una sincronización completa se conservan con
// estado DELETED para que las versiones publicadas sigan siendo auditables.
func SyncChannelTemplates(
	ctx context.Context,
	pool *pgxpool.Pool,
	wabaID string,
	templates []whatsapp.TemplateInfo,
	syncedAt time.Time,
) (*TemplateSyncReport, error) {
	if strings.TrimSpace(wabaID) == "" {
		return nil, errors.New("waba_id vacío")
	}
	syncedAt = syncedAt.UTC()
	marker := fmt.Sprintf("%d-%d", syncedAt.UnixNano(), len(templates))
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	for _, template := range templates {
		if template.Name == "" || template.Language == "" {
			return nil, errors.New("Meta devolvió una plantilla sin nombre o idioma")
		}
		components := normalizedComponents(template.Components)
		schema, bodyCount, unsupported := whatsapp.AnalyzeTemplateComponents(components, template.ParameterFormat)
		schemaJSON, _ := json.Marshal(schema)
		metaUpdatedAt := metaTimestamp(template.LastUpdatedTime)
		_, err = tx.Exec(ctx, `INSERT INTO channel_templates(
				waba_id,meta_template_id,name,language,status,category,quality_score,
				parameter_format,components,parameter_schema,body_parameter_count,
				has_unsupported_parameters,rejected_reason,pending_category,
				meta_updated_at,last_synced_at,sync_marker,updated_at)
			VALUES($1,NULLIF($2,''),$3,$4,COALESCE(NULLIF($5,''),'UNKNOWN'),NULLIF($6,''),
				NULLIF($7,''),NULLIF($8,''),$9::jsonb,$10::jsonb,$11,$12,NULLIF($13,''),
				NULLIF($14,''),$15,$16,$17,NOW())
			ON CONFLICT(waba_id,name,language) DO UPDATE SET
				meta_template_id=COALESCE(EXCLUDED.meta_template_id,channel_templates.meta_template_id),
				status=EXCLUDED.status,category=EXCLUDED.category,
				quality_score=EXCLUDED.quality_score,parameter_format=EXCLUDED.parameter_format,
				components=EXCLUDED.components,parameter_schema=EXCLUDED.parameter_schema,
				body_parameter_count=EXCLUDED.body_parameter_count,
				has_unsupported_parameters=EXCLUDED.has_unsupported_parameters,
				rejected_reason=EXCLUDED.rejected_reason,
				pending_category=EXCLUDED.pending_category,
				meta_updated_at=EXCLUDED.meta_updated_at,last_synced_at=EXCLUDED.last_synced_at,
				sync_marker=EXCLUDED.sync_marker,updated_at=NOW()`,
			wabaID, template.ID, template.Name, template.Language, strings.ToUpper(template.Status),
			strings.ToUpper(template.Category), strings.ToUpper(template.QualityScore.Score),
			strings.ToUpper(template.ParameterFormat), components, schemaJSON, bodyCount, unsupported,
			template.RejectedReason, strings.ToUpper(template.CorrectCategory), metaUpdatedAt, syncedAt, marker)
		if err != nil {
			return nil, err
		}
	}
	deleted, err := tx.Exec(ctx, `UPDATE channel_templates SET status='DELETED',
			pending_category=NULL,category_change_at=NULL,last_synced_at=$2,updated_at=NOW()
		WHERE waba_id=$1 AND sync_marker IS DISTINCT FROM $3 AND status <> 'DELETED'`,
		wabaID, syncedAt, marker)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE bots SET templates_synced_at=$2 WHERE waba_id=$1`, wabaID, syncedAt); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &TemplateSyncReport{Total: len(templates), MarkedDeleted: deleted.RowsAffected(), SyncedAt: syncedAt}, nil
}

func ListChannelTemplatesForBot(ctx context.Context, pool *pgxpool.Pool, botID string) ([]ChannelTemplate, error) {
	rows, err := pool.Query(ctx, `SELECT `+channelTemplateCols+` FROM channel_templates t
		JOIN bots b ON b.waba_id=t.waba_id
		WHERE b.id=$1::uuid ORDER BY t.name,t.language`, botID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ChannelTemplate])
}

func GetChannelTemplateForBot(
	ctx context.Context,
	pool *pgxpool.Pool,
	botID, name, language string,
) (*ChannelTemplate, error) {
	rows, err := pool.Query(ctx, `SELECT `+channelTemplateCols+` FROM channel_templates t
		JOIN bots b ON b.waba_id=t.waba_id
		WHERE b.id=$1::uuid AND t.name=$2 AND t.language=$3`, botID, name, language)
	if err != nil {
		return nil, err
	}
	template, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[ChannelTemplate])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &template, nil
}

// StoreAndApplyTemplateEvent persiste primero el evento y aplica después el
// estado. El last_event_at impide que un webhook atrasado revierta uno nuevo.
func StoreAndApplyTemplateEvent(
	ctx context.Context,
	pool *pgxpool.Pool,
	event whatsapp.TemplateEvent,
) (bool, error) {
	if event.EventKey == "" || event.WabaID == "" || event.Name == "" || event.Language == "" {
		return false, errors.New("evento de plantilla incompleto")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var eventID int64
	err = tx.QueryRow(ctx, `INSERT INTO channel_template_events(
			event_key,waba_id,field,meta_template_id,template_name,template_language,occurred_at,payload)
		VALUES($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8::jsonb)
		ON CONFLICT(event_key) DO NOTHING RETURNING id`,
		event.EventKey, event.WabaID, event.Field, event.TemplateID, event.Name,
		event.Language, event.OccurredAt, event.Payload).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	status, category, quality := "", "", ""
	pendingCategory := event.PendingCategory
	var categoryChangeAt *time.Time
	if event.Field == "message_template_status_update" {
		status, category = strings.ToUpper(event.Status), strings.ToUpper(event.Category)
	}
	if event.Field == "message_template_quality_update" {
		quality = strings.ToUpper(event.QualityScore)
	}
	if event.Field == "template_category_update" {
		category = strings.ToUpper(event.Category)
		pendingCategory = strings.ToUpper(pendingCategory)
		categoryChangeAt = event.CategoryChangeAt
	}
	rejectedReason := event.RejectionReason
	if rejectedReason == "" {
		rejectedReason = event.Reason
	}
	_, err = tx.Exec(ctx, `INSERT INTO channel_templates(
			waba_id,meta_template_id,name,language,status,category,quality_score,
			rejected_reason,pending_category,category_change_at,last_event_at,updated_at)
		VALUES($1,NULLIF($2,''),$3,$4,COALESCE(NULLIF($5,''),'UNKNOWN'),NULLIF($6,''),
			NULLIF($7,''),NULLIF($8,''),NULLIF($9,''),$10,$11,NOW())
		ON CONFLICT(waba_id,name,language) DO UPDATE SET
			meta_template_id=COALESCE(EXCLUDED.meta_template_id,channel_templates.meta_template_id),
			status=COALESCE(NULLIF(EXCLUDED.status,'UNKNOWN'),channel_templates.status),
			category=COALESCE(EXCLUDED.category,channel_templates.category),
			quality_score=COALESCE(EXCLUDED.quality_score,channel_templates.quality_score),
			rejected_reason=COALESCE(EXCLUDED.rejected_reason,channel_templates.rejected_reason),
			pending_category=CASE WHEN $12='template_category_update'
				THEN EXCLUDED.pending_category ELSE channel_templates.pending_category END,
			category_change_at=CASE WHEN $12='template_category_update'
				THEN EXCLUDED.category_change_at ELSE channel_templates.category_change_at END,
			last_event_at=EXCLUDED.last_event_at,updated_at=NOW()
		WHERE channel_templates.last_event_at IS NULL
			OR EXCLUDED.last_event_at >= channel_templates.last_event_at`,
		event.WabaID, event.TemplateID, event.Name, event.Language, status, category,
		quality, rejectedReason, pendingCategory, categoryChangeAt, event.OccurredAt, event.Field)
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE channel_template_events SET applied_at=NOW() WHERE id=$1`, eventID); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

type TemplateValidation struct {
	Warnings []string `json:"warnings"`
}

type flowTemplateQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// ValidateFlowTemplates valida únicamente capacidades dependientes del WABA.
// engine.Validate sigue siendo la fuente de verdad del grafo.
func ValidateFlowTemplates(
	ctx context.Context,
	pool *pgxpool.Pool,
	botID string,
	raw json.RawMessage,
) (*TemplateValidation, error) {
	return validateFlowTemplatesWithQuery(ctx, pool, botID, raw)
}

// validateFlowTemplatesWithQuery acepta tanto el pool como una pgx.Tx. Publicar
// lo usa con la transacción que ya bloqueó flows, de modo que nunca valide las
// plantillas de un draft distinto al que terminará versionando.
func validateFlowTemplatesWithQuery(
	ctx context.Context,
	q flowTemplateQueryer,
	botID string,
	raw json.RawMessage,
) (*TemplateValidation, error) {
	var flow engine.Flow
	if err := json.Unmarshal(raw, &flow); err != nil {
		return nil, err
	}
	result := &TemplateValidation{}
	if flow.Trigger.Type != "schedule" {
		return result, nil
	}
	botRows, err := q.Query(ctx, `SELECT `+botCols+` FROM bots WHERE id = $1::uuid`, botID)
	if err != nil {
		return nil, err
	}
	bot, err := pgx.CollectExactlyOneRow(botRows, pgx.RowToStructByName[Bot])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("bot no encontrado")
	}
	if err != nil {
		return nil, err
	}
	if bot.WabaID == nil || strings.TrimSpace(*bot.WabaID) == "" {
		return nil, errors.New("el bot no tiene un WABA asociado")
	}
	for _, node := range flow.Nodes {
		if node.Kind != "send" {
			continue
		}
		if strings.TrimSpace(node.TemplateLanguage) == "" {
			return nil, fmt.Errorf("nodo %s: la plantilla requiere idioma", node.ID)
		}
		templateRows, err := q.Query(ctx, `SELECT `+channelTemplateCols+` FROM channel_templates t
			JOIN bots b ON b.waba_id=t.waba_id
			WHERE b.id=$1::uuid AND t.name=$2 AND t.language=$3`,
			botID, node.TemplateName, node.TemplateLanguage)
		if err != nil {
			return nil, err
		}
		template, err := pgx.CollectExactlyOneRow(templateRows, pgx.RowToStructByName[ChannelTemplate])
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("nodo %s: la plantilla %s/%s no existe en el WABA; sincroniza el catálogo",
				node.ID, node.TemplateName, node.TemplateLanguage)
		}
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(template.Status, "APPROVED") {
			return nil, fmt.Errorf("nodo %s: la plantilla %s/%s está %s, no APPROVED",
				node.ID, node.TemplateName, node.TemplateLanguage, template.Status)
		}
		if template.HasUnsupportedParameters {
			return nil, fmt.Errorf("nodo %s: la plantilla usa parámetros de header, botón o con nombre que este sender aún no soporta", node.ID)
		}
		if len(node.TemplateParams) != template.BodyParameterCount {
			return nil, fmt.Errorf("nodo %s: la plantilla requiere %d parámetros BODY y el nodo define %d",
				node.ID, template.BodyParameterCount, len(node.TemplateParams))
		}
		category := ""
		if template.Category != nil {
			category = strings.ToUpper(*template.Category)
		}
		if category != "UTILITY" {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("nodo %s: la plantilla es %s, no UTILITY; puede tener mayor coste y reglas de opt-out distintas", node.ID, category))
		}
		if template.PendingCategory != nil && *template.PendingCategory != "" {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("nodo %s: Meta anunció una recategorización pendiente a %s", node.ID, *template.PendingCategory))
		}
		if template.QualityScore != nil && strings.EqualFold(*template.QualityScore, "RED") {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("nodo %s: la plantilla tiene calidad RED y está en riesgo de pausa", node.ID))
		}
	}
	return result, nil
}
