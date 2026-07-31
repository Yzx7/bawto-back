package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yzx7/sacs-chatbots/engine"
	"github.com/Yzx7/sacs-chatbots/models"
)

// Preview de un flujo `schedule` (§10.2 del plan): un ensayo en seco que responde
// a quién le llegaría, a quién no y por qué, sin escribir nada.
//
// Reutiliza deliberadamente las mismas funciones que el encolado y la entrega
// (ResolveDataViewAt, PrimaryContactForRecord, ReminderCapReached,
// ReminderChatBlock, engine.Advance). Un preview que calculara los destinatarios
// por su cuenta mentiría en cuanto una de las dos ramas cambiara.

// PreviewRecipient es un envío que hoy sí saldría.
type PreviewRecipient struct {
	RecordID    string   `json:"recordId"`
	ContactID   string   `json:"contactId"`
	ContactName string   `json:"contactName,omitempty"`
	Phone       string   `json:"phone"`
	Params      []string `json:"params,omitempty"`
}

// PreviewSkip explica una omisión. `Postponed` distingue lo que no saldría hoy
// pero sí más tarde (conversación en curso) de lo que no saldrá nunca.
type PreviewSkip struct {
	RecordID  string `json:"recordId"`
	ContactID string `json:"contactId,omitempty"`
	Reason    string `json:"reason"`
	Postponed bool   `json:"postponed,omitempty"`
}

// PreviewTemplate describe la plantilla con lo que importa para decidir enviar.
type PreviewTemplate struct {
	Name            string   `json:"name"`
	Language        string   `json:"language"`
	Status          string   `json:"status,omitempty"`
	Category        string   `json:"category,omitempty"`
	PendingCategory string   `json:"pendingCategory,omitempty"`
	QualityScore    string   `json:"qualityScore,omitempty"`
	Body            string   `json:"body,omitempty"`
	SampleParams    []string `json:"sampleParams,omitempty"`
	SampleRendered  string   `json:"sampleRendered,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`
}

// PreviewCost informa el volumen potencial antes de enviar. El importe se
// calcula en el reporte de consumo solo después de que Meta confirme entrega,
// billable y categoría: cobrar por una aceptación de API sería prematuro.
type PreviewCost struct {
	Category          string `json:"category,omitempty"`
	EstimatedBillable int    `json:"estimatedBillable"`
	ObservedRuns      int    `json:"observedRuns"`
	ObservedBillable  int    `json:"observedBillable"`
	Note              string `json:"note"`
}

// PreviewSchedule resume el estado del cron.
type PreviewSchedule struct {
	Cron            string      `json:"cron"`
	Timezone        string      `json:"timezone"`
	LastTickAt      *time.Time  `json:"lastTickAt,omitempty"`
	NextOccurrences []time.Time `json:"nextOccurrences"`
	CatchupPending  int         `json:"catchupPending"`
	CatchupDiscard  int         `json:"catchupDiscarded"`
	CatchupWindow   string      `json:"catchupWindow"`
}

// PreviewResult es la respuesta completa del endpoint.
type PreviewResult struct {
	At           time.Time                  `json:"at"`
	ViewID       string                     `json:"viewId"`
	ViewName     string                     `json:"viewName,omitempty"`
	RecordsFound int                        `json:"recordsFound"`
	Recipients   []PreviewRecipient         `json:"recipients"`
	Skipped      []PreviewSkip              `json:"skipped"`
	InvalidDates []models.InvalidDateRecord `json:"invalidDates"`
	Template     *PreviewTemplate           `json:"template,omitempty"`
	Schedule     PreviewSchedule            `json:"schedule"`
	Cost         PreviewCost                `json:"cost"`
	Truncated    bool                       `json:"truncated"`
}

// previewLimit acota el trabajo del preview. Una vista puede resolver 1000
// registros y cada uno consulta contacto, contexto y chat; pedirlos todos
// convertiría un botón de la UI en una consulta de varios segundos.
const previewLimit = 200

// NextOccurrences devuelve las próximas n ejecuciones a partir de `from`.
func NextOccurrences(expression string, loc *time.Location, from time.Time, n int) ([]time.Time, error) {
	schedule, err := cronParser.Parse(expression)
	if err != nil {
		return nil, err
	}
	if n <= 0 || n > 50 {
		n = 5
	}
	out := make([]time.Time, 0, n)
	cursor := from.In(loc)
	for len(out) < n {
		next := schedule.Next(cursor)
		if next.IsZero() {
			break
		}
		out = append(out, next.UTC())
		cursor = next
	}
	return out, nil
}

// ValidateCron comprueba una expresión sin necesidad de un flujo guardado.
func ValidateCron(expression string) error {
	_, err := cronParser.Parse(expression)
	return err
}

// PreviewFlow ejecuta el ensayo en seco. `definition` es el grafo a evaluar: el
// borrador cuando se está editando, la versión publicada cuando ya existe.
func PreviewFlow(
	ctx context.Context,
	pool *pgxpool.Pool,
	botID string,
	flow *models.Flow,
	definition json.RawMessage,
	catchupWindow time.Duration,
	at time.Time,
) (*PreviewResult, error) {
	var graph engine.Flow
	if err := json.Unmarshal(definition, &graph); err != nil {
		return nil, fmt.Errorf("el grafo no es JSON del editor: %w", err)
	}
	if graph.Trigger.Type != "schedule" {
		return nil, fmt.Errorf("solo se puede previsualizar un flujo schedule")
	}
	if strings.TrimSpace(graph.Trigger.ViewID) == "" {
		return nil, fmt.Errorf("el trigger no tiene una vista de datos seleccionada")
	}
	location := time.UTC
	if graph.Trigger.Timezone != "" {
		var err error
		location, err = time.LoadLocation(graph.Trigger.Timezone)
		if err != nil {
			return nil, fmt.Errorf("timezone inválida: %w", err)
		}
	}
	at = at.UTC().Truncate(time.Minute)

	result := &PreviewResult{
		At: at, ViewID: graph.Trigger.ViewID,
		Recipients: []PreviewRecipient{}, Skipped: []PreviewSkip{},
		InvalidDates: []models.InvalidDateRecord{},
		Schedule: PreviewSchedule{
			Cron: graph.Trigger.Cron, Timezone: graph.Trigger.Timezone,
			LastTickAt: flow.LastTickAt, NextOccurrences: []time.Time{},
			CatchupWindow: catchupWindow.String(),
		},
	}
	if view, err := models.GetDataViewForBot(ctx, pool, botID, graph.Trigger.ViewID); err != nil {
		return nil, err
	} else if view == nil {
		return nil, fmt.Errorf("la vista de datos no existe en esta organización")
	} else {
		result.ViewName = view.Name
	}

	if err := fillSchedulePreview(&result.Schedule, graph.Trigger.Cron, location, catchupWindow, at); err != nil {
		return nil, err
	}

	invalid, err := models.InvalidDateRecordsForView(ctx, pool, botID, graph.Trigger.ViewID, 100)
	if err != nil {
		return nil, err
	}
	result.InvalidDates = invalid

	records, err := models.ResolveDataViewAt(ctx, pool, botID, graph.Trigger.ViewID, at.In(location))
	if err != nil {
		return nil, err
	}
	result.RecordsFound = len(records)
	if len(records) > previewLimit {
		records = records[:previewLimit]
		result.Truncated = true
	}

	for _, record := range records {
		skip, recipient, err := previewRecord(ctx, pool, botID, flow, graph, record, at)
		if err != nil {
			return nil, err
		}
		if skip != nil {
			result.Skipped = append(result.Skipped, *skip)
			continue
		}
		result.Recipients = append(result.Recipients, *recipient)
	}

	template, err := previewTemplate(ctx, pool, botID, graph, result.Recipients)
	if err != nil {
		return nil, err
	}
	result.Template = template

	cost, err := previewCost(ctx, pool, flow.ID, template, len(result.Recipients))
	if err != nil {
		return nil, err
	}
	result.Cost = cost
	return result, nil
}

func fillSchedulePreview(out *PreviewSchedule, expression string, loc *time.Location, window time.Duration, at time.Time) error {
	if strings.TrimSpace(expression) == "" {
		return fmt.Errorf("el trigger no tiene expresión cron")
	}
	next, err := NextOccurrences(expression, loc, at, 5)
	if err != nil {
		return fmt.Errorf("cron inválido: %w", err)
	}
	out.NextOccurrences = next
	if out.LastTickAt == nil {
		return nil
	}
	// Un flujo que lleva parado acumula ocurrencias: cuántas se recuperarían y
	// cuántas se han pasado ya de la ventana es justo lo que nadie ve hoy.
	pending, err := Occurrences(expression, loc, *out.LastTickAt, at)
	if err != nil {
		return fmt.Errorf("cron inválido: %w", err)
	}
	limit := at.Add(-window)
	for _, occurrence := range pending {
		if occurrence.Before(limit) {
			out.CatchupDiscard++
		} else {
			out.CatchupPending++
		}
	}
	return nil
}

// previewRecord decide el destino de un registro. Devuelve (skip, nil) o (nil, recipient).
func previewRecord(
	ctx context.Context,
	pool *pgxpool.Pool,
	botID string,
	flow *models.Flow,
	graph engine.Flow,
	record models.DataRecord,
	at time.Time,
) (*PreviewSkip, *PreviewRecipient, error) {
	contact, err := models.PrimaryContactForRecord(ctx, pool, botID, record.ID)
	if err != nil {
		return nil, nil, err
	}
	if contact == nil {
		return &PreviewSkip{RecordID: record.ID, Reason: "sin contacto vinculado"}, nil, nil
	}
	if contact.Status != "active" {
		return &PreviewSkip{RecordID: record.ID, ContactID: contact.ID,
			Reason: "contacto " + contact.Status}, nil, nil
	}
	if flow.PublishedVersionID != nil {
		capped, err := models.ReminderCapReached(ctx, pool, flow.ID, record.ID, 3)
		if err != nil {
			return nil, nil, err
		}
		if capped {
			return &PreviewSkip{RecordID: record.ID, ContactID: contact.ID,
				Reason: "límite de recordatorios alcanzado"}, nil, nil
		}
	}
	reason, err := models.ReminderChatBlock(ctx, pool, botID, contact.PhoneNormalized)
	if err != nil {
		return nil, nil, err
	}
	if reason != "" {
		return &PreviewSkip{RecordID: record.ID, ContactID: contact.ID,
			Reason: reason, Postponed: true}, nil, nil
	}

	vars, err := models.DataRecordContext(ctx, pool, botID, record.ID)
	if err != nil {
		return nil, nil, err
	}
	if vars == nil {
		vars = map[string]string{}
	}
	vars["contact_id"] = contact.ID
	vars["contact_phone"] = contact.PhoneNormalized
	name := ""
	if contact.Name != nil {
		name = *contact.Name
		vars["contact_name"] = name
	}
	recipient := &PreviewRecipient{RecordID: record.ID, ContactID: contact.ID,
		ContactName: name, Phone: contact.PhoneNormalized}
	// El mismo Advance que usa la entrega: si el grafo no resuelve, el operador
	// lo ve aquí y no en un run muerto.
	execution, err := engine.Advance(&graph, nil, "", engine.Deps{Context: vars})
	if err == nil && len(execution.Templates) == 1 {
		recipient.Params = execution.Templates[0].Params
	}
	return nil, recipient, nil
}

func previewTemplate(
	ctx context.Context,
	pool *pgxpool.Pool,
	botID string,
	graph engine.Flow,
	recipients []PreviewRecipient,
) (*PreviewTemplate, error) {
	var node *engine.Node
	for i := range graph.Nodes {
		if graph.Nodes[i].Kind == "send" && graph.Nodes[i].TemplateName != "" {
			node = &graph.Nodes[i]
			break
		}
	}
	if node == nil {
		return nil, nil
	}
	out := &PreviewTemplate{Name: node.TemplateName, Language: node.TemplateLanguage}
	if len(recipients) > 0 {
		out.SampleParams = recipients[0].Params
	}
	template, err := models.GetChannelTemplateForBot(ctx, pool, botID, node.TemplateName, node.TemplateLanguage)
	if err != nil {
		return nil, err
	}
	if template == nil {
		out.Warnings = append(out.Warnings,
			"la plantilla no está en el catálogo local; sincroniza con Meta antes de publicar")
		return out, nil
	}
	out.Status = template.Status
	if template.Category != nil {
		out.Category = strings.ToUpper(*template.Category)
	}
	if template.PendingCategory != nil {
		out.PendingCategory = *template.PendingCategory
	}
	if template.QualityScore != nil {
		out.QualityScore = *template.QualityScore
	}
	out.Body = templateBody(template.Components)
	out.SampleRendered = renderTemplateBody(out.Body, out.SampleParams)
	if !strings.EqualFold(template.Status, "APPROVED") {
		out.Warnings = append(out.Warnings, "la plantilla está "+template.Status+", no APPROVED: no se puede enviar")
	}
	if out.Category != "" && out.Category != "UTILITY" {
		out.Warnings = append(out.Warnings, "la plantilla es "+out.Category+
			", no UTILITY: mayor coste y reglas de opt-out distintas")
	}
	if out.PendingCategory != "" {
		out.Warnings = append(out.Warnings, "Meta anunció una recategorización pendiente a "+out.PendingCategory)
	}
	if strings.EqualFold(out.QualityScore, "RED") {
		out.Warnings = append(out.Warnings, "la plantilla tiene calidad RED y está en riesgo de pausa")
	}
	if len(node.TemplateParams) != template.BodyParameterCount {
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"la plantilla espera %d parámetros BODY y el nodo define %d",
			template.BodyParameterCount, len(node.TemplateParams)))
	}
	return out, nil
}

// templateBody extrae el texto del componente BODY del catálogo.
func templateBody(components json.RawMessage) string {
	var parsed []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(components, &parsed) != nil {
		return ""
	}
	for _, component := range parsed {
		if strings.EqualFold(component.Type, "BODY") {
			return component.Text
		}
	}
	return ""
}

// renderTemplateBody sustituye los {{1}}..{{n}} de Meta por los parámetros del
// primer destinatario, para que el operador lea el mensaje tal como llegará.
func renderTemplateBody(body string, params []string) string {
	if body == "" || len(params) == 0 {
		return body
	}
	for i, param := range params {
		body = strings.ReplaceAll(body, fmt.Sprintf("{{%d}}", i+1), param)
	}
	return body
}

func previewCost(ctx context.Context, pool *pgxpool.Pool, flowID string, template *PreviewTemplate, recipients int) (PreviewCost, error) {
	cost := PreviewCost{
		EstimatedBillable: recipients,
		Note: "Meta cobra por plantilla entregada, no por conversación. Este es el máximo potencial; " +
			"el importe aparece en Consumo cuando el webhook confirma entrega, categoría y si fue facturable.",
	}
	if template != nil {
		cost.Category = template.Category
	}
	err := pool.QueryRow(ctx, `SELECT COUNT(*)::int,
		COUNT(*) FILTER (WHERE billable)::int FROM flow_runs
		WHERE flow_id=$1::uuid AND status IN ('sent','delivered','read','played')`,
		flowID).Scan(&cost.ObservedRuns, &cost.ObservedBillable)
	return cost, err
}
