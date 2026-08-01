// Package scheduler ejecuta flujos programados con semántica durable e
// idempotente. La unicidad de run_key es la garantía de corrección; el advisory
// lock solo evita que dos instancias hagan el mismo trabajo de descubrimiento.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"github.com/Yzx7/sacs-chatbots/channels/whatsapp"
	"github.com/Yzx7/sacs-chatbots/config"
	"github.com/Yzx7/sacs-chatbots/engine"
	"github.com/Yzx7/sacs-chatbots/events"
	"github.com/Yzx7/sacs-chatbots/helpers"
	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/robfig/cron/v3"
)

const schedulerAdvisoryLock int64 = 0x424157544F02

type Report struct{ Queued, Skipped, Total int }

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// Occurrences enumera los instantes exactos del cron en (lastTick, now].
func Occurrences(expression string, loc *time.Location, lastTick, now time.Time) ([]time.Time, error) {
	schedule, err := cronParser.Parse(expression)
	if err != nil {
		return nil, err
	}
	cursor := lastTick.In(loc)
	end := now.In(loc)
	out := make([]time.Time, 0)
	for len(out) < 10000 {
		next := schedule.Next(cursor)
		if next.After(end) {
			return out, nil
		}
		out = append(out, next.UTC())
		cursor = next
	}
	return nil, fmt.Errorf("cron produjo más de 10000 ocurrencias pendientes")
}

// QueueFlow resuelve la vista de la versión publicada y congela el contexto.
// `source` marca de dónde vino el disparo: "schedule" el cron, "manual" el panel.
func QueueFlow(ctx context.Context, pool *pgxpool.Pool, scheduled models.ScheduledFlow, at time.Time, source string) (Report, error) {
	var flow engine.Flow
	if err := json.Unmarshal(scheduled.Definition, &flow); err != nil || flow.Trigger.Type != "schedule" || flow.Trigger.ViewID == "" {
		return Report{}, fmt.Errorf("flujo schedule inválido o sin viewId")
	}
	location := time.UTC
	if flow.Trigger.Timezone != "" {
		var err error
		location, err = time.LoadLocation(flow.Trigger.Timezone)
		if err != nil {
			return Report{}, fmt.Errorf("timezone inválida: %w", err)
		}
	}
	records, err := models.ResolveDataViewAt(ctx, pool, scheduled.BotID, flow.Trigger.ViewID, at.In(location))
	if err != nil {
		return Report{}, err
	}
	report := Report{Total: len(records)}
	for _, record := range records {
		contact, err := models.PrimaryContactForRecord(ctx, pool, scheduled.BotID, record.ID)
		if err != nil || contact == nil || contact.Status != "active" {
			report.Skipped++
			continue
		}
		vars, err := models.DataRecordContext(ctx, pool, scheduled.BotID, record.ID)
		if err != nil {
			return report, err
		}
		vars["contact_id"] = contact.ID
		vars["contact_phone"] = contact.PhoneNormalized
		if contact.Name != nil {
			vars["contact_name"] = *contact.Name
		}
		raw, _ := json.Marshal(vars)
		key := fmt.Sprintf("%s:%s:%s:%s:%s", scheduled.FlowID, scheduled.FlowVersionID,
			record.ID, contact.ID, at.UTC().Truncate(time.Minute).Format(time.RFC3339))
		_, created, err := models.CreateFlowRun(ctx, pool, scheduled.BotID, scheduled.FlowID,
			scheduled.FlowVersionID, record.ID, contact.ID, key, source, at, raw)
		if err != nil {
			return report, err
		}
		if created {
			report.Queued++
		} else {
			report.Skipped++
		}
	}
	return report, nil
}

func Start(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, log *slog.Logger) {
	go func() {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			process(ctx, pool, cfg, log, time.Now().UTC())
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}

func process(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, log *slog.Logger, now time.Time) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		log.Error("scheduler acquire", "err", err)
		return
	}
	defer conn.Release()
	var leader bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, schedulerAdvisoryLock).Scan(&leader); err != nil || !leader {
		return
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, schedulerAdvisoryLock)
	}()

	flows, err := models.ListPublishedScheduleFlows(ctx, pool)
	if err != nil {
		log.Error("scheduler list flows", "err", err)
		return
	}
	for _, scheduled := range flows {
		var flow engine.Flow
		if err := json.Unmarshal(scheduled.Definition, &flow); err != nil {
			log.Error("scheduler definition", "flow", scheduled.FlowID, "err", err)
			continue
		}
		loc := time.UTC
		if flow.Trigger.Timezone != "" {
			loc, err = time.LoadLocation(flow.Trigger.Timezone)
			if err != nil {
				log.Error("scheduler timezone", "flow", scheduled.FlowID, "err", err)
				continue
			}
		}
		if scheduled.LastTickAt == nil {
			_ = models.AdvanceFlowTick(ctx, pool, scheduled.FlowID, now)
			continue
		}
		occurrences, err := Occurrences(flow.Trigger.Cron, loc, *scheduled.LastTickAt, now)
		if err != nil {
			log.Error("scheduler cron", "flow", scheduled.FlowID, "err", err)
			continue
		}
		ok := true
		for _, occurrence := range occurrences {
			if occurrence.Before(now.Add(-cfg.SchedulerCatchupWindow)) {
				_ = models.RecordScheduleOccurrence(ctx, pool, scheduled.FlowID, scheduled.FlowVersionID,
					occurrence, "skipped", "fuera de ventana de catch-up", 0, 0)
				continue
			}
			report, qerr := QueueFlow(ctx, pool, scheduled, occurrence, "schedule")
			if qerr != nil {
				ok = false
				_ = models.RecordScheduleOccurrence(ctx, pool, scheduled.FlowID, scheduled.FlowVersionID,
					occurrence, "failed", qerr.Error(), 0, 0)
				log.Error("scheduler queue", "flow", scheduled.FlowID, "at", occurrence, "err", qerr)
				break
			}
			_ = models.RecordScheduleOccurrence(ctx, pool, scheduled.FlowID, scheduled.FlowVersionID,
				occurrence, "processed", "", report.Queued, report.Skipped)
		}
		if ok {
			_ = models.AdvanceFlowTick(ctx, pool, scheduled.FlowID, now)
		}
	}
}

func StartDelivery(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, cipher *helpers.Cipher, hub *events.Hub, log *slog.Logger) {
	if cipher == nil {
		log.Warn("delivery scheduler disabled: cipher unavailable")
		return
	}
	host, _ := os.Hostname()
	workerID := fmt.Sprintf("%s:%d", host, os.Getpid())
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			if recovered, err := models.RecoverStaleFlowRuns(ctx, pool, time.Now().Add(-cfg.SchedulerLockTimeout)); err == nil && recovered > 0 {
				log.Warn("delivery recovered stale runs", "count", recovered)
			}
			for deliverOne(ctx, pool, cfg, cipher, hub, log, workerID) {
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}

type deliveryResult struct {
	providerID string
	wabaID     string
	cancel     string
	postpone   string
	err        error
}

func deliverOne(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, cipher *helpers.Cipher, hub *events.Hub, log *slog.Logger, workerID string) bool {
	run, err := models.ClaimFlowRun(ctx, pool, workerID)
	if err != nil {
		log.Error("delivery claim", "err", err)
		return false
	}
	if run == nil {
		return false
	}
	result := deliver(ctx, pool, cfg, cipher, hub, run)
	switch {
	case result.cancel != "":
		_ = models.CancelFlowRun(ctx, pool, run.ID, result.cancel)
	case result.postpone != "":
		_ = models.PostponeFlowRun(ctx, pool, run.ID, result.postpone, time.Now().Add(cfg.SchedulerChatPostpone), 3)
	case result.err == nil:
		_ = models.FinishFlowRun(ctx, pool, run.ID, result.providerID)
	default:
		classifyDeliveryError(ctx, pool, run, result.wabaID, result.err, log)
	}
	return true
}

func classifyDeliveryError(ctx context.Context, pool *pgxpool.Pool, run *models.FlowRun, wabaID string, cause error, log *slog.Logger) {
	var ambiguous *whatsapp.AmbiguousSendError
	if errors.As(cause, &ambiguous) {
		_ = models.UnverifyFlowRun(ctx, pool, run.ID, cause.Error())
		log.Error("delivery unverified", "run", run.ID, "err", cause)
		return
	}
	var api *whatsapp.APIError
	if errors.As(cause, &api) {
		code := fmt.Sprint(api.StatusCode)
		if api.StatusCode == 408 || api.StatusCode == 429 || api.StatusCode >= 500 {
			delay := retryDelay(run.Attempt)
			if api.RetryAfter > delay {
				delay = api.RetryAfter
			}
			if api.StatusCode == 429 && wabaID != "" {
				_ = models.PauseWABA(ctx, pool, wabaID, time.Now().Add(delay))
			}
			_ = models.RetryFlowRun(ctx, pool, run.ID, code, "transient", cause.Error(), time.Now().Add(delay))
			return
		}
		_ = models.FailFlowRun(ctx, pool, run.ID, code, "permanent", cause.Error())
		return
	}
	_ = models.RetryFlowRun(ctx, pool, run.ID, "", "transient", cause.Error(), time.Now().Add(retryDelay(run.Attempt)))
}

func retryDelay(attempt int) time.Duration {
	delays := []time.Duration{0, time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}
	i := attempt - 1
	if i < 0 {
		i = 0
	}
	if i >= len(delays) {
		i = len(delays) - 1
	}
	if delays[i] == 0 {
		return 0
	}
	return time.Duration(float64(delays[i]) * (0.5 + rand.Float64()))
}

func deliver(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, cipher *helpers.Cipher, hub *events.Hub, run *models.FlowRun) deliveryResult {
	definition, err := models.RunnableFlowDefinition(ctx, pool, run)
	if err != nil {
		return deliveryResult{err: err}
	}
	if definition == nil {
		return deliveryResult{cancel: "flujo ya no está publicado"}
	}
	if run.ContactID == nil || run.DataRecordID == nil {
		return deliveryResult{cancel: "contacto o registro eliminado"}
	}
	var flow engine.Flow
	if err := json.Unmarshal(definition, &flow); err != nil {
		return deliveryResult{err: fmt.Errorf("flujo inválido: %w", err)}
	}
	location := time.UTC
	if flow.Trigger.Timezone != "" {
		location, err = time.LoadLocation(flow.Trigger.Timezone)
		if err != nil {
			return deliveryResult{cancel: "timezone inválida"}
		}
	}
	stillInView, err := models.RecordStillInViewAt(ctx, pool, run.BotID, flow.Trigger.ViewID,
		*run.DataRecordID, time.Now().In(location))
	if err != nil {
		return deliveryResult{err: err}
	}
	if !stillInView {
		return deliveryResult{cancel: "registro ya no cumple la vista"}
	}
	capped, err := models.ReminderCapReached(ctx, pool, run.FlowID, *run.DataRecordID, 3)
	if err != nil {
		return deliveryResult{err: err}
	}
	if capped {
		return deliveryResult{cancel: "límite de recordatorios alcanzado"}
	}
	contact, err := models.GetContact(ctx, pool, run.BotID, *run.ContactID)
	if err != nil {
		return deliveryResult{err: err}
	}
	if contact == nil || contact.Status != "active" {
		return deliveryResult{cancel: "contacto inactivo, bloqueado o eliminado"}
	}
	if reason, err := models.ReminderChatBlock(ctx, pool, run.BotID, contact.PhoneNormalized); err != nil {
		return deliveryResult{err: err}
	} else if reason != "" {
		return deliveryResult{postpone: reason}
	}
	bot, err := models.GetBot(ctx, pool, run.BotID)
	if err != nil || bot == nil {
		return deliveryResult{err: fmt.Errorf("bot no encontrado")}
	}
	if bot.ChannelID == nil || bot.WabaID == nil || *bot.ChannelID == "" || *bot.WabaID == "" {
		return deliveryResult{cancel: "canal sin phone_number_id o waba_id"}
	}
	channel, err := models.GetBotByChannel(ctx, pool, whatsapp.Channel, *bot.ChannelID)
	if err != nil || channel == nil {
		return deliveryResult{err: fmt.Errorf("canal no encontrado")}
	}
	token, err := cipher.Decrypt(channel.TokenEnc)
	if err != nil {
		return deliveryResult{err: err}
	}
	vars := map[string]string{}
	_ = json.Unmarshal(run.Context, &vars)
	execution, err := engine.Advance(&flow, nil, "", engine.Deps{Context: vars})
	if err != nil {
		return deliveryResult{err: err}
	}
	if len(execution.Sends) > 0 || len(execution.Templates) != 1 {
		return deliveryResult{cancel: "un flow schedule debe producir exactamente una plantilla"}
	}
	template := execution.Templates[0]
	sendCfg := whatsapp.SendConfig{APIBase: cfg.WhatsAppAPIBase, Version: cfg.WhatsAppAPIVersion,
		PhoneNumberID: *bot.ChannelID, Token: token}
	status, err := whatsapp.GetTemplateStatus(ctx, sendCfg, *bot.WabaID, template.Name, template.Language)
	if err != nil {
		return deliveryResult{wabaID: *bot.WabaID, err: err}
	}
	if status == nil || !strings.EqualFold(status.Status, "APPROVED") {
		return deliveryResult{cancel: "plantilla no existe o no está aprobada"}
	}
	// La categoría se valida y advierte al publicar. Meta permite también
	// MARKETING/AUTHENTICATION en schedules; el worker no debe convertir una
	// advertencia explícitamente aceptada en una cancelación silenciosa.
	spacing := time.Duration(float64(time.Second) / cfg.SchedulerWABAMPS)
	wait, err := models.ReserveWABASlot(ctx, pool, *bot.WabaID, spacing)
	if err != nil {
		return deliveryResult{err: err}
	}
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return deliveryResult{err: ctx.Err()}
		case <-timer.C:
		}
	}
	providerID, err := whatsapp.SendTemplate(ctx, sendCfg, contact.PhoneNormalized, template.Name, template.Language, template.Params)
	if err != nil {
		return deliveryResult{wabaID: *bot.WabaID, err: err}
	}
	if err := models.AttachFlowRunProviderMessage(ctx, pool, run.ID, providerID); err != nil {
		return deliveryResult{providerID: providerID, wabaID: *bot.WabaID, err: &whatsapp.AmbiguousSendError{Err: err}}
	}
	contactName := ""
	if contact.Name != nil {
		contactName = *contact.Name
	}
	chatID, err := models.UpsertChat(ctx, pool, run.BotID, contact.PhoneNormalized, contactName)
	if err != nil {
		return deliveryResult{providerID: providerID, wabaID: *bot.WabaID, err: &whatsapp.AmbiguousSendError{Err: err}}
	}
	metadata, _ := json.Marshal(map[string]string{
		"flowId": run.FlowID, "flowVersionId": run.FlowVersionID, "flowRunId": run.ID,
		"dataRecordId": *run.DataRecordID, "templateName": template.Name,
	})
	body := "[Plantilla " + template.Name + "]"
	if len(template.Params) > 0 {
		body += " " + strings.Join(template.Params, " · ")
	}
	message, err := models.InsertOutboundMessageWithMetadata(ctx, pool, chatID, providerID, "template", body, metadata)
	if err != nil {
		return deliveryResult{providerID: providerID, wabaID: *bot.WabaID, err: &whatsapp.AmbiguousSendError{Err: err}}
	}
	if message != nil && hub != nil {
		raw, _ := json.Marshal(message)
		hub.Publish(ctx, events.Event{Type: "message", BotID: run.BotID, ChatID: chatID, Payload: raw})
	}
	if updates, err := models.ReconcileProviderStatusEvents(ctx, pool, providerID); err == nil && hub != nil {
		for _, update := range updates {
			raw, _ := json.Marshal(update)
			hub.Publish(ctx, events.Event{Type: "message_status", BotID: update.BotID, ChatID: update.ChatID, Payload: raw})
		}
	}
	return deliveryResult{providerID: providerID, wabaID: *bot.WabaID}
}
