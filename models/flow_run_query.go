package models

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Historial de ejecuciones (§10.3 y §11.4 del plan). Todo se acota por bot_id:
// el id del run nunca basta como autorización, igual que en el resto del backend.

var (
	// ErrRunNotRetryable: un run vivo se reintenta solo, y reencolarlo a mano
	// mientras un worker lo tiene tomado duplicaría el envío.
	ErrRunNotRetryable = errors.New("solo se puede reintentar un run terminado (failed, dead, cancelled o unverified)")
	// ErrRunNotCancellable: un run ya entregado o en curso no se cancela. Con
	// `running` el mensaje puede estar ya camino a Meta; cancelarlo daría una
	// falsa sensación de haberlo detenido.
	ErrRunNotCancellable = errors.New("solo se puede cancelar un run en espera (queued o retry_wait)")
)

// FlowRunListItem añade al run lo mínimo para leer el historial sin N+1:
// de qué flujo es y a quién iba dirigido.
type FlowRunListItem struct {
	FlowRun
	FlowKey      string  `db:"flow_key" json:"flowKey"`
	FlowName     string  `db:"flow_name" json:"flowName"`
	CancelReason *string `db:"cancel_reason" json:"cancelReason,omitempty"`
	ContactName  *string `db:"contact_name" json:"contactName,omitempty"`
	ContactPhone *string `db:"contact_phone" json:"contactPhone,omitempty"`
}

// FlowRunFilter refleja los filtros de §10.3.
type FlowRunFilter struct {
	FlowID    string
	Statuses  []string
	From      *time.Time
	To        *time.Time
	ContactID string
	RecordID  string
	ErrorCode string
	Limit     int
	Offset    int
}

// FlowRunPage devuelve la página junto al total, para que la UI pueda paginar
// sin adivinar si hay más.
type FlowRunPage struct {
	Items  []FlowRunListItem `json:"items"`
	Total  int               `json:"total"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
}

// FlowRunStatuses son los estados válidos, en el orden del ciclo de vida (§8.1).
var FlowRunStatuses = []string{
	"queued", "running", "retry_wait", "sent", "delivered", "read", "played",
	"failed", "dead", "unverified", "cancelled",
}

func validRunStatus(status string) bool {
	for _, s := range FlowRunStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// runFilterSQL construye el WHERE compartido por la lista y el contador, para
// que ambos no puedan divergir.
func runFilterSQL(botID string, f FlowRunFilter) (string, []any) {
	where := ` WHERE r.bot_id = $1::uuid`
	args := []any{botID}
	add := func(clause string, value any) {
		args = append(args, value)
		where += strings.Replace(clause, "?", "$"+strconv.Itoa(len(args)), 1)
	}
	if f.FlowID != "" {
		add(` AND r.flow_id = ?::uuid`, f.FlowID)
	}
	if len(f.Statuses) > 0 {
		add(` AND r.status = ANY(?)`, f.Statuses)
	}
	if f.From != nil {
		add(` AND r.scheduled_for >= ?`, f.From.UTC())
	}
	if f.To != nil {
		add(` AND r.scheduled_for < ?`, f.To.UTC())
	}
	if f.ContactID != "" {
		add(` AND r.contact_id = ?::uuid`, f.ContactID)
	}
	if f.RecordID != "" {
		add(` AND r.data_record_id = ?::uuid`, f.RecordID)
	}
	if f.ErrorCode != "" {
		add(` AND r.last_error_code = ?`, f.ErrorCode)
	}
	return where, args
}

const flowRunListCols = `r.id::text AS id, r.bot_id::text AS bot_id, r.flow_id::text AS flow_id,
	r.flow_version_id::text AS flow_version_id, r.data_record_id::text AS data_record_id,
	r.contact_id::text AS contact_id, r.run_key, r.status, r.scheduled_for, r.source, r.attempt,
	r.max_attempts, r.postponement_count, r.next_attempt_at, r.provider_message_id, r.context,
	r.provider_status_at, r.delivered_at, r.read_at, r.played_at, r.conversation_id,
	r.conversation_type, r.pricing_model, r.pricing_type, r.pricing_category, r.billable,
	r.last_error_code, r.last_error_class, r.last_error, r.created_at, r.started_at, r.finished_at,
	r.cancel_reason, f.key AS flow_key, f.name AS flow_name,
	c.name AS contact_name, c.phone_normalized AS contact_phone`

// ListFlowRuns pagina el historial del bot, del más reciente al más antiguo.
func ListFlowRuns(ctx context.Context, p *pgxpool.Pool, botID string, f FlowRunFilter) (*FlowRunPage, error) {
	for _, status := range f.Statuses {
		if !validRunStatus(status) {
			return nil, errors.New("estado de run desconocido: " + status)
		}
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	where, args := runFilterSQL(botID, f)

	var total int
	if err := p.QueryRow(ctx, `SELECT COUNT(*)::int FROM flow_runs r`+where, args...).Scan(&total); err != nil {
		return nil, err
	}
	page := &FlowRunPage{Items: []FlowRunListItem{}, Total: total, Limit: f.Limit, Offset: f.Offset}
	if total == 0 || f.Offset >= total {
		return page, nil
	}
	args = append(args, f.Limit, f.Offset)
	query := `SELECT ` + flowRunListCols + ` FROM flow_runs r
		JOIN flows f ON f.id = r.flow_id
		LEFT JOIN contacts c ON c.id = r.contact_id` + where +
		` ORDER BY r.scheduled_for DESC, r.created_at DESC
		LIMIT $` + strconv.Itoa(len(args)-1) + ` OFFSET $` + strconv.Itoa(len(args))
	rows, err := p.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByName[FlowRunListItem])
	if err != nil {
		return nil, err
	}
	page.Items = items
	return page, nil
}

// FlowRunStatusCount alimenta los contadores por estado de la pantalla.
type FlowRunStatusCount struct {
	Status string `json:"status"`
	Count  int    `json:"count"`
}

// CountFlowRunsByStatus respeta los mismos filtros salvo el propio estado: los
// contadores deben seguir viéndose todos al filtrar por uno.
func CountFlowRunsByStatus(ctx context.Context, p *pgxpool.Pool, botID string, f FlowRunFilter) ([]FlowRunStatusCount, error) {
	f.Statuses = nil
	where, args := runFilterSQL(botID, f)
	rows, err := p.Query(ctx, `SELECT r.status, COUNT(*)::int AS count FROM flow_runs r`+where+
		` GROUP BY r.status`, args...)
	if err != nil {
		return nil, err
	}
	counts, err := pgx.CollectRows(rows, pgx.RowToStructByName[FlowRunStatusCount])
	if err != nil {
		return nil, err
	}
	return counts, nil
}

// FlowRunReply es la respuesta del cliente correlacionada con el recordatorio.
type FlowRunReply struct {
	MessageID      int64     `json:"messageId"`
	ChatID         string    `json:"chatId"`
	Body           *string   `json:"body,omitempty"`
	Type           string    `json:"type"`
	Method         string    `json:"method"`
	CandidateCount int       `json:"candidateCount"`
	CreatedAt      time.Time `json:"createdAt"`
}

// FlowRunDetail es lo que necesita §11.4 para explicar un run sin abrir la base.
type FlowRunDetail struct {
	FlowRunListItem
	FlowVersion   *int            `json:"flowVersion,omitempty"`
	TemplateName  string          `json:"templateName,omitempty"`
	TemplateLang  string          `json:"templateLanguage,omitempty"`
	RecordData    json.RawMessage `json:"recordData,omitempty"`
	RecordMissing bool            `json:"recordMissing"`
	Reply         *FlowRunReply   `json:"reply,omitempty"`
}

// GetFlowRun devuelve el detalle, o nil si el run no es de este bot.
func GetFlowRun(ctx context.Context, p *pgxpool.Pool, botID, runID string) (*FlowRunDetail, error) {
	rows, err := p.Query(ctx, `SELECT `+flowRunListCols+` FROM flow_runs r
		JOIN flows f ON f.id = r.flow_id
		LEFT JOIN contacts c ON c.id = r.contact_id
		WHERE r.id = $1::uuid AND r.bot_id = $2::uuid`, runID, botID)
	if err != nil {
		return nil, err
	}
	item, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowRunListItem])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	detail := &FlowRunDetail{FlowRunListItem: item}

	// La plantilla se lee de la versión congelada, no del borrador: es la que
	// se envió (o se enviará) realmente.
	var version int
	var definition json.RawMessage
	err = p.QueryRow(ctx, `SELECT version, definition FROM flow_versions WHERE id = $1::uuid`,
		item.FlowVersionID).Scan(&version, &definition)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		detail.FlowVersion = &version
		detail.TemplateName, detail.TemplateLang = firstTemplateOfDefinition(definition)
	}

	if item.DataRecordID != nil {
		var data json.RawMessage
		err = p.QueryRow(ctx, `SELECT r.data FROM data_records r
			JOIN data_objects o ON o.id = r.object_id
			WHERE r.id = $1::uuid AND o.org_id = (SELECT org_id FROM bots WHERE id = $2::uuid)`,
			*item.DataRecordID, botID).Scan(&data)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			detail.RecordMissing = true
		case err != nil:
			return nil, err
		default:
			detail.RecordData = data
		}
	} else {
		detail.RecordMissing = true
	}

	var reply FlowRunReply
	err = p.QueryRow(ctx, `SELECT m.id, m.chat_id::text, m.body, m.type, mc.method,
		mc.candidate_count, m.created_at
		FROM message_correlations mc JOIN messages m ON m.id = mc.inbound_message_id
		WHERE mc.flow_run_id = $1::uuid ORDER BY m.created_at LIMIT 1`, item.ID).
		Scan(&reply.MessageID, &reply.ChatID, &reply.Body, &reply.Type, &reply.Method,
			&reply.CandidateCount, &reply.CreatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		detail.Reply = &reply
	}
	return detail, nil
}

// firstTemplateOfDefinition extrae la plantilla del único `send` de un flujo
// schedule (§3.6). Devuelve vacío si el grafo no tiene ninguna.
func firstTemplateOfDefinition(definition json.RawMessage) (string, string) {
	var doc struct {
		Nodes []struct {
			Kind             string `json:"kind"`
			TemplateName     string `json:"templateName"`
			TemplateLanguage string `json:"templateLanguage"`
		} `json:"nodes"`
	}
	if json.Unmarshal(definition, &doc) != nil {
		return "", ""
	}
	for _, node := range doc.Nodes {
		if node.Kind == "send" && node.TemplateName != "" {
			return node.TemplateName, node.TemplateLanguage
		}
	}
	return "", ""
}

// RequeueFlowRun reencola un run terminado. Devuelve ErrRunNotRetryable si el
// estado no lo permite, y nil si el run no pertenece al bot.
//
// El intento vuelve a 0 a propósito: el backoff mide fallos consecutivos del
// worker, y un reintento manual es una decisión nueva, no la continuación de
// la anterior. `provider_message_id` se conserva para no perder la pista del
// envío anterior en un run `unverified`.
func RequeueFlowRun(ctx context.Context, p *pgxpool.Pool, botID, runID, userID string) (*FlowRunDetail, error) {
	tag, err := p.Exec(ctx, `UPDATE flow_runs SET status='queued', attempt=0,
		next_attempt_at=NOW(), finished_at=NULL, started_at=NULL, cancel_reason=NULL,
		last_error=NULL, last_error_code=NULL, last_error_class=NULLIF($3,''),
		locked_at=NULL, locked_by=NULL, heartbeat_at=NULL
		WHERE id=$1::uuid AND bot_id=$2::uuid
		  AND status IN ('failed','dead','cancelled','unverified')`,
		runID, botID, requeueNote(userID))
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		exists, err := flowRunExists(ctx, p, botID, runID)
		if err != nil || !exists {
			return nil, err
		}
		return nil, ErrRunNotRetryable
	}
	return GetFlowRun(ctx, p, botID, runID)
}

// requeueNote deja rastro de que el reencolado fue manual; last_error_class es
// el único campo libre que ya lee la UI para explicar el estado.
func requeueNote(userID string) string {
	if userID == "" {
		return "requeued_manual"
	}
	return "requeued_manual:" + userID
}

// CancelFlowRunForBot cancela un run en espera. Devuelve ErrRunNotCancellable
// si ya está en curso o terminado, y nil si el run no pertenece al bot.
func CancelFlowRunForBot(ctx context.Context, p *pgxpool.Pool, botID, runID, reason string) (*FlowRunDetail, error) {
	if strings.TrimSpace(reason) == "" {
		reason = "cancelado manualmente"
	}
	tag, err := p.Exec(ctx, `UPDATE flow_runs SET status='cancelled', cancel_reason=$3,
		finished_at=NOW(), locked_at=NULL, locked_by=NULL, heartbeat_at=NULL
		WHERE id=$1::uuid AND bot_id=$2::uuid AND status IN ('queued','retry_wait')`,
		runID, botID, reason)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		exists, err := flowRunExists(ctx, p, botID, runID)
		if err != nil || !exists {
			return nil, err
		}
		return nil, ErrRunNotCancellable
	}
	return GetFlowRun(ctx, p, botID, runID)
}

func flowRunExists(ctx context.Context, p *pgxpool.Pool, botID, runID string) (bool, error) {
	var exists bool
	err := p.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM flow_runs WHERE id=$1::uuid AND bot_id=$2::uuid)`,
		runID, botID).Scan(&exists)
	return exists, err
}

// ScheduleOccurrence es una pasada del scheduler sobre un flujo, para mostrar
// en la UI que una ocurrencia se procesó, se saltó o falló.
type ScheduleOccurrence struct {
	ScheduledFor time.Time `db:"scheduled_for" json:"scheduledFor"`
	Status       string    `db:"status" json:"status"`
	Reason       *string   `db:"reason" json:"reason,omitempty"`
	QueuedCount  int       `db:"queued_count" json:"queuedCount"`
	SkippedCount int       `db:"skipped_count" json:"skippedCount"`
	CreatedAt    time.Time `db:"created_at" json:"createdAt"`
}

// ListScheduleOccurrences devuelve las últimas pasadas del flujo, acotadas por bot.
func ListScheduleOccurrences(ctx context.Context, p *pgxpool.Pool, botID, flowID string, limit int) ([]ScheduleOccurrence, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := p.Query(ctx, `SELECT o.scheduled_for, o.status, o.reason, o.queued_count,
		o.skipped_count, o.created_at FROM flow_schedule_occurrences o
		WHERE o.flow_id = (SELECT id FROM flows WHERE id=$1::uuid AND bot_id=$2::uuid)
		ORDER BY o.scheduled_for DESC LIMIT $3`, flowID, botID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ScheduleOccurrence])
}
