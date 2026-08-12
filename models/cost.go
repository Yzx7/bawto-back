package models

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	metaPricingProvider = "meta_whatsapp"
	metaPricingSource   = "https://whatsappbusiness.com/es-la/products/platform-pricing/#rates"
)

// AIUsageEventInput es una llamada completada por el proveedor de IA. Los
// precios se copian en la fila: un cambio futuro de modelo no reescribe costos
// históricos.
type AIUsageEventInput struct {
	OrganizationID   string
	BotID            string
	ChatID           string
	InboundMessageID int64
	// Purpose separa el gasto del bot del que genera el editor. Un caller viejo
	// conserva el significado histórico `flow_runtime`; no puede inventar otras
	// categorías porque la base también las restringe.
	Purpose                  string
	Provider                 string
	Model                    string
	ProviderRequestID        string
	InputTokens              int64
	OutputTokens             int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
	InputUSDPerMillion       float64
	OutputUSDPerMillion      float64
	CacheReadUSDPerMillion   float64
	CacheWriteUSDPerMillion  float64
	Outcome                  string
	Metadata                 json.RawMessage
}

// RecordAIUsage es idempotente por proveedor + request id. Un request id vacío
// se admite para proveedores compatibles que no lo entreguen.
func RecordAIUsage(ctx context.Context, pool *pgxpool.Pool, in AIUsageEventInput) error {
	if strings.TrimSpace(in.Provider) == "" || strings.TrimSpace(in.Model) == "" {
		return fmt.Errorf("usage IA sin proveedor o modelo")
	}
	if in.Outcome == "" {
		in.Outcome = "ok"
	}
	if in.Purpose == "" {
		in.Purpose = "flow_runtime"
	}
	if in.Purpose != "flow_runtime" && in.Purpose != "flow_authoring" {
		return fmt.Errorf("purpose de usage IA inválido: %s", in.Purpose)
	}
	if len(in.Metadata) == 0 {
		in.Metadata = json.RawMessage(`{}`)
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO ai_usage_events
		    (organization_id,bot_id,chat_id,inbound_message_id,purpose,provider,model,
		     provider_request_id,input_tokens,output_tokens,cache_read_input_tokens,
		     cache_creation_input_tokens,input_usd_per_million,output_usd_per_million,
		     cache_read_usd_per_million,cache_write_usd_per_million,outcome,metadata)
		VALUES ($1::uuid,NULLIF($2,'')::uuid,NULLIF($3,'')::uuid,NULLIF($4,0),
		        $5,$6,$7,NULLIF($8,''),$9,$10,$11,$12,$13,$14,$15,$16,$17,$18::jsonb)
		ON CONFLICT (provider,provider_request_id)
		    WHERE provider_request_id IS NOT NULL DO NOTHING`,
		in.OrganizationID, in.BotID, in.ChatID, in.InboundMessageID, in.Purpose,
		in.Provider, in.Model, in.ProviderRequestID, in.InputTokens, in.OutputTokens,
		in.CacheReadInputTokens, in.CacheCreationInputTokens,
		in.InputUSDPerMillion, in.OutputUSDPerMillion,
		in.CacheReadUSDPerMillion, in.CacheWriteUSDPerMillion,
		in.Outcome, in.Metadata)
	return err
}

type CostReport struct {
	From       time.Time           `json:"from"`
	To         time.Time           `json:"to"`
	WhatsApp   WhatsAppCostSummary `json:"whatsApp"`
	AI         AIUsageSummary      `json:"ai"`
	Copilot    CopilotUsageSummary `json:"copilot"`
	Disclaimer string              `json:"disclaimer"`
}

type WhatsAppCostSummary struct {
	Delivered        int64                  `json:"delivered"`
	Billable         int64                  `json:"billable"`
	Free             int64                  `json:"free"`
	UnpricedBillable int64                  `json:"unpricedBillable"`
	EstimatedCost    float64                `json:"estimatedCost"`
	Currency         string                 `json:"currency"`
	Basis            string                 `json:"basis"`
	SourceURL        string                 `json:"sourceUrl"`
	ByCategory       []WhatsAppCategoryCost `json:"byCategory"`
	ActiveRateCard   []ProviderRateTier     `json:"activeRateCard"`
}

type WhatsAppCategoryCost struct {
	Market           string  `json:"market"`
	Category         string  `json:"category"`
	Delivered        int64   `json:"delivered"`
	Billable         int64   `json:"billable"`
	Free             int64   `json:"free"`
	UnpricedBillable int64   `json:"unpricedBillable"`
	EstimatedCost    float64 `json:"estimatedCost"`
	Currency         string  `json:"currency,omitempty"`
}

type ProviderRateTier struct {
	Market        string     `json:"market"`
	Currency      string     `json:"currency"`
	Category      string     `json:"category"`
	TierFrom      int64      `json:"tierFrom"`
	TierTo        *int64     `json:"tierTo,omitempty"`
	UnitPrice     float64    `json:"unitPrice"`
	EffectiveFrom time.Time  `json:"effectiveFrom"`
	EffectiveTo   *time.Time `json:"effectiveTo,omitempty"`
	SourceURL     string     `json:"sourceUrl"`
}

type AIUsageSummary struct {
	Requests                 int64          `json:"requests"`
	InputTokens              int64          `json:"inputTokens"`
	OutputTokens             int64          `json:"outputTokens"`
	CacheReadInputTokens     int64          `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64          `json:"cacheCreationInputTokens"`
	TotalTokens              int64          `json:"totalTokens"`
	EstimatedCostUSD         float64        `json:"estimatedCostUsd"`
	Currency                 string         `json:"currency"`
	ByModel                  []AIModelUsage `json:"byModel"`
}

type AIModelUsage struct {
	Provider                 string  `json:"provider"`
	Model                    string  `json:"model"`
	Requests                 int64   `json:"requests"`
	InputTokens              int64   `json:"inputTokens"`
	OutputTokens             int64   `json:"outputTokens"`
	CacheReadInputTokens     int64   `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64   `json:"cacheCreationInputTokens"`
	TotalTokens              int64   `json:"totalTokens"`
	EstimatedCostUSD         float64 `json:"estimatedCostUsd"`
}

// CopilotUsageSummary es el consumo del asistente de autoría, mantenido aparte
// del runtime: todo se filtra por purpose='flow_authoring', así que nunca se
// mezcla con la IA que atiende a los clientes.
type CopilotUsageSummary struct {
	Requests         int64   `json:"requests"`
	Turns            int64   `json:"turns"`
	Sessions         int64   `json:"sessions"`
	EstimatedCostUSD float64 `json:"estimatedCostUsd"`
	Currency         string  `json:"currency"`
}

// GetCostReport combina dos fuentes independientes:
//   - WhatsApp: entrega + billable/categoría confirmados por webhook, valorizados
//     con el tarifario público que corresponda a fecha, mercado y nivel mensual.
//   - IA: usage exacto informado por el proveedor y la tarifa copiada al evento.
//
// botID vacío produce el total de la organización. Nunca suma PEN con USD.
func GetCostReport(ctx context.Context, pool *pgxpool.Pool, orgID, botID string, from, to time.Time) (*CostReport, error) {
	if !from.Before(to) {
		return nil, fmt.Errorf("el inicio debe ser anterior al fin")
	}
	report := &CostReport{
		From: from.UTC(),
		To:   to.UTC(),
		WhatsApp: WhatsAppCostSummary{
			Currency:   "PEN",
			Basis:      "estimated_list_rate",
			SourceURL:  metaPricingSource,
			ByCategory: make([]WhatsAppCategoryCost, 0),
		},
		AI:      AIUsageSummary{Currency: "USD", ByModel: make([]AIModelUsage, 0)},
		Copilot: CopilotUsageSummary{Currency: "USD"},
		Disclaimer: "WhatsApp es una estimación de tarifa pública basada en entregas y pricing del webhook; " +
			"la factura puede variar por descuentos del portafolio, impuestos o ajustes de Meta. " +
			"IA usa tokens reales de la respuesta y la tarifa configurada para ese request.",
	}
	if err := loadWhatsAppCost(ctx, pool, report, orgID, botID, from.UTC(), to.UTC()); err != nil {
		return nil, err
	}
	if err := loadAIUsage(ctx, pool, report, orgID, botID, from.UTC(), to.UTC()); err != nil {
		return nil, err
	}
	if err := loadCopilotUsage(ctx, pool, report, orgID, botID, from.UTC(), to.UTC()); err != nil {
		return nil, err
	}
	tiers, err := activeProviderRateTiers(ctx, pool, metaPricingProvider, "PE", to.Add(-time.Nanosecond))
	if err != nil {
		return nil, err
	}
	report.WhatsApp.ActiveRateCard = tiers
	return report, nil
}

// La consulta usa toda la porción mensual anterior al rango para calcular el
// ordinal del mensaje dentro del portafolio comercial. Así un reporte de media
// quincena no vuelve artificialmente al primer nivel del tarifario.
func loadWhatsAppCost(ctx context.Context, pool *pgxpool.Pool, report *CostReport, orgID, botID string, from, to time.Time) error {
	monthStart := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC)
	rows, err := pool.Query(ctx, `
		WITH delivery AS (
		    SELECT DISTINCT ON (e.provider_message_id)
		        e.provider_message_id,
		        b.org_id,
		        c.bot_id,
		        COALESCE(NULLIF(b.business_id,''),NULLIF(b.waba_id,''),b.org_id::text) AS portfolio_key,
		        COALESCE(NULLIF(m.pricing_category,''),NULLIF(e.pricing_category,''),'unknown') AS category,
		        COALESCE(m.billable,e.billable,FALSE) AS billable,
		        e.occurred_at,
		        CASE
		            WHEN regexp_replace(COALESCE(NULLIF(e.recipient_id,''),c.contact),'[^0-9]','','g') LIKE '51%'
		            THEN 'PE'
		            ELSE 'UNKNOWN'
		        END AS market
		    FROM provider_status_events e
		    JOIN messages m ON m.wa_id=e.provider_message_id
		    JOIN chats c ON c.id=m.chat_id
		    JOIN bots b ON b.id=c.bot_id
		    WHERE e.status IN ('delivered','read','played')
		      AND e.occurred_at >= $1 AND e.occurred_at < $2
		    ORDER BY e.provider_message_id,
		        CASE e.status WHEN 'delivered' THEN 1 WHEN 'read' THEN 2 ELSE 3 END,
		        e.occurred_at,e.id
		), ranked AS (
		    SELECT d.*,
		        COUNT(*) FILTER (WHERE billable) OVER (
		            PARTITION BY portfolio_key,market,category,date_trunc('month',occurred_at)
		            ORDER BY occurred_at,provider_message_id
		            ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
		        ) AS billable_ordinal
		    FROM delivery d
		), valued AS (
		    SELECT r.*,
		        rc.currency,
		        CASE WHEN r.billable THEN rc.unit_price ELSE 0 END AS estimated_cost,
		        CASE WHEN r.billable AND rc.id IS NULL THEN 1 ELSE 0 END AS unpriced
		    FROM ranked r
		    LEFT JOIN provider_rate_cards rc
		      ON rc.provider=$3
		     AND rc.market=r.market
		     AND rc.category=LOWER(r.category)
		     AND r.billable_ordinal >= rc.tier_from
		     AND (rc.tier_to IS NULL OR r.billable_ordinal <= rc.tier_to)
		     AND r.occurred_at::date >= rc.effective_from
		     AND (rc.effective_to IS NULL OR r.occurred_at::date < rc.effective_to)
		)
		SELECT market,LOWER(category),
		       COUNT(*)::bigint,
		       COUNT(*) FILTER (WHERE billable)::bigint,
		       COUNT(*) FILTER (WHERE NOT billable)::bigint,
		       COALESCE(SUM(unpriced),0)::bigint,
		       COALESCE(SUM(estimated_cost),0)::float8,
		       COALESCE(MAX(currency),'')
		  FROM valued
		 WHERE org_id=$4::uuid
		   AND ($5='' OR bot_id=$5::uuid)
		   AND occurred_at >= $6 AND occurred_at < $2
		 GROUP BY market,LOWER(category)
		 ORDER BY market,LOWER(category)`,
		monthStart, to, metaPricingProvider, orgID, botID, from)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var item WhatsAppCategoryCost
		if err := rows.Scan(&item.Market, &item.Category, &item.Delivered, &item.Billable,
			&item.Free, &item.UnpricedBillable, &item.EstimatedCost, &item.Currency); err != nil {
			return err
		}
		item.EstimatedCost = roundMoney(item.EstimatedCost)
		report.WhatsApp.ByCategory = append(report.WhatsApp.ByCategory, item)
		report.WhatsApp.Delivered += item.Delivered
		report.WhatsApp.Billable += item.Billable
		report.WhatsApp.Free += item.Free
		report.WhatsApp.UnpricedBillable += item.UnpricedBillable
		report.WhatsApp.EstimatedCost += item.EstimatedCost
	}
	report.WhatsApp.EstimatedCost = roundMoney(report.WhatsApp.EstimatedCost)
	return rows.Err()
}

func loadAIUsage(ctx context.Context, pool *pgxpool.Pool, report *CostReport, orgID, botID string, from, to time.Time) error {
	rows, err := pool.Query(ctx, `
		SELECT provider,model,COUNT(*)::bigint,
		       COALESCE(SUM(input_tokens),0)::bigint,
		       COALESCE(SUM(output_tokens),0)::bigint,
		       COALESCE(SUM(cache_read_input_tokens),0)::bigint,
		       COALESCE(SUM(cache_creation_input_tokens),0)::bigint,
		       COALESCE(SUM(estimated_cost_usd),0)::float8
		  FROM ai_usage_events
		 WHERE organization_id=$1::uuid
		   AND ($2='' OR bot_id=$2::uuid)
		   AND purpose='flow_runtime'
		   AND occurred_at >= $3 AND occurred_at < $4
		 GROUP BY provider,model
		 ORDER BY provider,model`, orgID, botID, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item AIModelUsage
		if err := rows.Scan(&item.Provider, &item.Model, &item.Requests, &item.InputTokens,
			&item.OutputTokens, &item.CacheReadInputTokens, &item.CacheCreationInputTokens,
			&item.EstimatedCostUSD); err != nil {
			return err
		}
		item.TotalTokens = item.InputTokens + item.OutputTokens +
			item.CacheReadInputTokens + item.CacheCreationInputTokens
		item.EstimatedCostUSD = roundCostUSD(item.EstimatedCostUSD)
		report.AI.ByModel = append(report.AI.ByModel, item)
		report.AI.Requests += item.Requests
		report.AI.InputTokens += item.InputTokens
		report.AI.OutputTokens += item.OutputTokens
		report.AI.CacheReadInputTokens += item.CacheReadInputTokens
		report.AI.CacheCreationInputTokens += item.CacheCreationInputTokens
		report.AI.EstimatedCostUSD += item.EstimatedCostUSD
	}
	report.AI.TotalTokens = report.AI.InputTokens + report.AI.OutputTokens +
		report.AI.CacheReadInputTokens + report.AI.CacheCreationInputTokens
	report.AI.EstimatedCostUSD = roundCostUSD(report.AI.EstimatedCostUSD)
	return rows.Err()
}

// loadCopilotUsage agrega el consumo del asistente de autoría en el periodo. El
// costo sale de ai_usage_events filtrado por purpose='flow_authoring'; los
// turnos y sesiones salen de las tablas del Copilot. Nada de esto toca las
// métricas del runtime.
func loadCopilotUsage(ctx context.Context, pool *pgxpool.Pool, report *CostReport, orgID, botID string, from, to time.Time) error {
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)::bigint,
		       COALESCE(SUM(estimated_cost_usd),0)::float8
		  FROM ai_usage_events
		 WHERE organization_id=$1::uuid
		   AND ($2='' OR bot_id=$2::uuid)
		   AND purpose='flow_authoring'
		   AND occurred_at >= $3 AND occurred_at < $4`, orgID, botID, from, to).
		Scan(&report.Copilot.Requests, &report.Copilot.EstimatedCostUSD); err != nil {
		return err
	}
	report.Copilot.EstimatedCostUSD = roundCostUSD(report.Copilot.EstimatedCostUSD)

	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)::bigint
		  FROM flow_copilot_turns t
		  JOIN flow_copilot_sessions s ON s.id=t.session_id
		 WHERE t.organization_id=$1::uuid
		   AND ($2='' OR s.bot_id=$2::uuid)
		   AND t.created_at >= $3 AND t.created_at < $4`, orgID, botID, from, to).
		Scan(&report.Copilot.Turns); err != nil {
		return err
	}

	return pool.QueryRow(ctx, `
		SELECT COUNT(*)::bigint
		  FROM flow_copilot_sessions
		 WHERE organization_id=$1::uuid
		   AND ($2='' OR bot_id=$2::uuid)
		   AND created_at >= $3 AND created_at < $4`, orgID, botID, from, to).
		Scan(&report.Copilot.Sessions)
}

func activeProviderRateTiers(ctx context.Context, pool *pgxpool.Pool, provider, market string, at time.Time) ([]ProviderRateTier, error) {
	rows, err := pool.Query(ctx, `
		SELECT market,currency,category,tier_from,tier_to,unit_price::float8,
		       effective_from::timestamptz,effective_to::timestamptz,source_url
		  FROM provider_rate_cards
		 WHERE provider=$1 AND market=$2
		   AND $3::date >= effective_from
		   AND (effective_to IS NULL OR $3::date < effective_to)
		 ORDER BY category,tier_from`, provider, market, at)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ProviderRateTier, 0)
	for rows.Next() {
		var tier ProviderRateTier
		if err := rows.Scan(&tier.Market, &tier.Currency, &tier.Category, &tier.TierFrom,
			&tier.TierTo, &tier.UnitPrice, &tier.EffectiveFrom, &tier.EffectiveTo,
			&tier.SourceURL); err != nil {
			return nil, err
		}
		out = append(out, tier)
	}
	return out, rows.Err()
}

func roundMoney(value float64) float64 {
	return math.Round(value*10_000) / 10_000
}

func roundCostUSD(value float64) float64 {
	return math.Round(value*1_000_000_0000) / 1_000_000_0000
}
