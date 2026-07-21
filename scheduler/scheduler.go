// Package scheduler convierte triggers schedule en flow_runs idempotentes.
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/Yzx7/sacs-chatbots/channels/whatsapp"
	"github.com/Yzx7/sacs-chatbots/config"
	"github.com/Yzx7/sacs-chatbots/engine"
	"github.com/Yzx7/sacs-chatbots/helpers"
	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Report struct{ Queued, Skipped, Total int }

// QueueFlow resuelve la vista del trigger y reserva un run por registro/contacto.
func QueueFlow(ctx context.Context, pool *pgxpool.Pool, bot *models.Bot, now time.Time) (Report, error) {
	var flow engine.Flow
	if json.Unmarshal(bot.Flow, &flow) != nil || flow.Trigger.Type != "schedule" || flow.Trigger.ViewID == "" {
		return Report{}, fmt.Errorf("el bot no tiene un flujo schedule con viewId")
	}
	loc := time.UTC
	if flow.Trigger.Timezone != "" {
		var err error
		loc, err = time.LoadLocation(flow.Trigger.Timezone)
		if err != nil {
			return Report{}, fmt.Errorf("zona horaria inválida: %w", err)
		}
	}
	records, err := models.ResolveDataView(ctx, pool, bot.ID, flow.Trigger.ViewID)
	if err != nil {
		return Report{}, err
	}
	report := Report{Total: len(records)}
	flowID := flow.ID
	if flowID == "" {
		flowID = "default"
	}
	dateKey := now.In(loc).Format("2006-01-02")
	for _, record := range records {
		contact, err := models.PrimaryContactForRecord(ctx, pool, bot.ID, record.ID)
		if err != nil || contact == nil {
			report.Skipped++
			continue
		}
		vars, err := models.DataRecordContext(ctx, pool, bot.ID, record.ID)
		if err != nil {
			return report, err
		}
		vars["contact_id"] = contact.ID
		vars["contact_phone"] = contact.PhoneNormalized
		if contact.Name != nil {
			vars["contact_name"] = *contact.Name
		}
		raw, _ := json.Marshal(vars)
		key := fmt.Sprintf("%s:%s:%s:%s", flowID, record.ID, contact.ID, dateKey)
		_, created, err := models.CreateFlowRun(ctx, pool, bot.ID, flowID, record.ID, contact.ID, key, raw)
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

// Start revisa cada minuto los flows schedule. La idempotencia en flow_runs
// permite que dos ticks o dos instancias no dupliquen ejecuciones.
func Start(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) {
	go func() {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			process(ctx, pool, log, time.Now())
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}

// StartDelivery consume runs encolados. Requiere template nodes: un flow
// programado nunca usa texto libre fuera de la ventana de 24 h.
func StartDelivery(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, cipher *helpers.Cipher, log *slog.Logger) {
	if cipher == nil {
		log.Warn("delivery scheduler disabled: cipher unavailable")
		return
	}
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			for deliverOne(ctx, pool, cfg, cipher, log) {
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}
func deliverOne(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, cipher *helpers.Cipher, log *slog.Logger) bool {
	run, err := models.ClaimFlowRun(ctx, pool)
	if err != nil {
		log.Error("delivery claim", "err", err)
		return false
	}
	if run == nil {
		return false
	}
	if err = deliver(ctx, pool, cfg, cipher, run); err != nil {
		_ = models.FailFlowRun(ctx, pool, run.ID, err)
		log.Error("delivery run", "run", run.ID, "err", err)
		return true
	}
	_ = models.FinishFlowRun(ctx, pool, run.ID)
	return true
}
func deliver(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, cipher *helpers.Cipher, run *models.FlowRun) error {
	bot, err := models.GetBot(ctx, pool, run.BotID)
	if err != nil || bot == nil {
		return fmt.Errorf("bot no encontrado")
	}
	var flow engine.Flow
	if json.Unmarshal(bot.Flow, &flow) != nil {
		return fmt.Errorf("flujo inválido")
	}
	vars := map[string]string{}
	_ = json.Unmarshal(run.Context, &vars)
	result, err := engine.Advance(&flow, nil, "", engine.Deps{Context: vars})
	if err != nil {
		return err
	}
	if len(result.Sends) > 0 {
		return fmt.Errorf("un flow schedule debe usar nodos template, no texto libre")
	}
	if len(result.Templates) == 0 {
		return fmt.Errorf("el flow schedule no produjo una plantilla")
	}
	if bot.ChannelID == nil || *bot.ChannelID == "" {
		return fmt.Errorf("bot sin canal conectado")
	}
	channel, err := models.GetBotByChannel(ctx, pool, whatsapp.Channel, *bot.ChannelID)
	if err != nil || channel == nil {
		return fmt.Errorf("canal no encontrado")
	}
	token, err := cipher.Decrypt(channel.TokenEnc)
	if err != nil {
		return err
	}
	contact, err := models.GetContact(ctx, pool, bot.ID, run.ContactID)
	if err != nil || contact == nil {
		return fmt.Errorf("contacto no encontrado")
	}
	sendCfg := whatsapp.SendConfig{APIBase: cfg.WhatsAppAPIBase, Version: cfg.WhatsAppAPIVersion, PhoneNumberID: *bot.ChannelID, Token: token}
	for _, t := range result.Templates {
		if _, err := whatsapp.SendTemplate(ctx, sendCfg, contact.PhoneNormalized, t.Name, t.Language, t.Params); err != nil {
			return err
		}
	}
	return nil
}

func process(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, now time.Time) {
	bots, err := models.ListScheduledBots(ctx, pool)
	if err != nil {
		log.Error("scheduler list bots", "err", err)
		return
	}
	for i := range bots {
		var flow engine.Flow
		if json.Unmarshal(bots[i].Flow, &flow) != nil {
			continue
		}
		loc := time.UTC
		if flow.Trigger.Timezone != "" {
			var e error
			loc, e = time.LoadLocation(flow.Trigger.Timezone)
			if e != nil {
				log.Warn("scheduler timezone", "bot", bots[i].ID, "err", e)
				continue
			}
		}
		if !CronMatches(flow.Trigger.Cron, now.In(loc)) {
			continue
		}
		if _, err := QueueFlow(ctx, pool, &bots[i], now); err != nil {
			log.Error("scheduler queue", "bot", bots[i].ID, "err", err)
		}
	}
}

// CronMatches admite cron de cinco campos: *, números, listas, rangos y pasos
// (por ejemplo: 0 9 1 * * o */15 8-18 * * 1-5).
func CronMatches(cron string, at time.Time) bool {
	parts := strings.Fields(cron)
	if len(parts) != 5 {
		return false
	}
	values := []int{at.Minute(), at.Hour(), at.Day(), int(at.Month()), int(at.Weekday())}
	ranges := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	for i, spec := range parts {
		if !cronFieldMatches(spec, values[i], ranges[i][0], ranges[i][1]) {
			return false
		}
	}
	return true
}

func cronFieldMatches(spec string, value, min, max int) bool {
	for _, part := range strings.Split(spec, ",") {
		base, step := part, 1
		if before, after, ok := strings.Cut(part, "/"); ok {
			base = before
			var err error
			step, err = strconv.Atoi(after)
			if err != nil || step < 1 {
				continue
			}
		}
		lo, hi := min, max
		if base != "*" {
			if a, b, ok := strings.Cut(base, "-"); ok {
				var e1, e2 error
				lo, e1 = strconv.Atoi(a)
				hi, e2 = strconv.Atoi(b)
				if e1 != nil || e2 != nil {
					continue
				}
			} else {
				n, e := strconv.Atoi(base)
				if e != nil {
					continue
				}
				lo, hi = n, n
			}
		}
		if value >= lo && value <= hi && (value-lo)%step == 0 {
			return true
		}
	}
	return false
}
