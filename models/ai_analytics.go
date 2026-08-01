package models

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Anchos del histograma. Se elige por rango para que la serie no pase de ~100
// puntos: un mes por hora son 744 barras que ni se leen ni se dibujan gratis.
const (
	bucketHour = "hour"
	bucketDay  = "day"
	bucketWeek = "week"
)

// topChatsLimit acota el desglose por conversación. Sin tope, un bot con miles
// de contactos devolvería una respuesta que el panel no puede pintar.
const topChatsLimit = 10

// Los cuatro contadores son consumo real facturable por el proveedor: la
// lectura de caché es más barata que la entrada fresca, pero no es gratis.
const aiTotalTokensExpr = "(e.input_tokens+e.output_tokens+e.cache_read_input_tokens+e.cache_creation_input_tokens)"

// El filtro es idéntico en las seis consultas y todas alias la tabla como `e`.
// Se comparte como constante para que un cambio de alcance no se aplique a unas
// sí y a otras no, que daría cifras incoherentes dentro del mismo panel.
const aiUsageFilter = `e.organization_id=$1::uuid
	   AND ($2='' OR e.bot_id=$2::uuid)
	   AND e.occurred_at >= $3 AND e.occurred_at < $4`

// AIUsageAnalytics es el detalle de consumo de tokens de un periodo. No sustituye
// a CostReport: aquel valoriza la factura, este explica de dónde sale el gasto.
type AIUsageAnalytics struct {
	From     time.Time       `json:"from"`
	To       time.Time       `json:"to"`
	Bucket   string          `json:"bucket"`
	Currency string          `json:"currency"`
	Series   []AIUsageBucket `json:"series"`
	PerCall  AIPerCallStats  `json:"perCall"`
	ByNode   []AINodeUsage   `json:"byNode"`
	BySteps  []AIStepsUsage  `json:"bySteps"`
	TopChats []AIChatUsage   `json:"topChats"`
	Wasted   AIWastedUsage   `json:"wasted"`
}

// AIUsageBucket es un punto del histograma. Los huecos se rellenan con ceros:
// una serie que salta del día 3 al día 9 se dibuja como si no hubiera pasado
// nada entre medias, y eso oculta justo la caída que interesa ver.
type AIUsageBucket struct {
	StartsAt                 time.Time `json:"startsAt"`
	Requests                 int64     `json:"requests"`
	InputTokens              int64     `json:"inputTokens"`
	OutputTokens             int64     `json:"outputTokens"`
	CacheReadInputTokens     int64     `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64     `json:"cacheCreationInputTokens"`
	TotalTokens              int64     `json:"totalTokens"`
	Chats                    int64     `json:"chats"`
	EstimatedCostUSD         float64   `json:"estimatedCostUsd"`
}

// AIPerCallStats describe el reparto por llamada. La media sola engaña cuando
// unas pocas conversaciones largas la arrastran, así que va con p50 y p95.
type AIPerCallStats struct {
	Requests        int64   `json:"requests"`
	AvgTotalTokens  float64 `json:"avgTotalTokens"`
	AvgInputTokens  float64 `json:"avgInputTokens"`
	AvgOutputTokens float64 `json:"avgOutputTokens"`
	P50TotalTokens  float64 `json:"p50TotalTokens"`
	P95TotalTokens  float64 `json:"p95TotalTokens"`
	MaxTotalTokens  int64   `json:"maxTotalTokens"`
	AvgCostUSD      float64 `json:"avgCostUsd"`
	AvgDurationMs   float64 `json:"avgDurationMs"`
	P95DurationMs   float64 `json:"p95DurationMs"`
	// CacheHitRatio es lectura de caché sobre todo el contexto de entrada. Es la
	// palanca más barata que existe: subirlo baja la factura sin tocar el flujo.
	CacheHitRatio float64 `json:"cacheHitRatio"`
}

// AINodeUsage reparte el gasto entre los nodos IA del grafo. NodeID vacío es una
// llamada registrada antes de que el metadata llevara el nodo.
type AINodeUsage struct {
	NodeID           string  `json:"nodeId"`
	Requests         int64   `json:"requests"`
	TotalTokens      int64   `json:"totalTokens"`
	AvgTotalTokens   float64 `json:"avgTotalTokens"`
	EstimatedCostUSD float64 `json:"estimatedCostUsd"`
}

// AIStepsUsage es la distribución del bucle agéntico. Un turno sin herramientas
// cuesta 1 petición; con ellas puede costar hasta maxAgentSteps, y ese número es
// el que explica una factura que sube sin que suba el tráfico.
type AIStepsUsage struct {
	Steps            int     `json:"steps"`
	Requests         int64   `json:"requests"`
	TotalTokens      int64   `json:"totalTokens"`
	AvgTotalTokens   float64 `json:"avgTotalTokens"`
	EstimatedCostUSD float64 `json:"estimatedCostUsd"`
}

type AIChatUsage struct {
	ChatID           string    `json:"chatId"`
	Contact          string    `json:"contact"`
	ContactName      string    `json:"contactName,omitempty"`
	Requests         int64     `json:"requests"`
	TotalTokens      int64     `json:"totalTokens"`
	AvgTotalTokens   float64   `json:"avgTotalTokens"`
	EstimatedCostUSD float64   `json:"estimatedCostUsd"`
	LastAt           time.Time `json:"lastAt"`
}

// AIWastedUsage son tokens pagados que no produjeron respuesta útil. Se separan
// porque su remedio es distinto: el prompt o el esquema de salida, no el tráfico.
type AIWastedUsage struct {
	InvalidOutputRequests int64   `json:"invalidOutputRequests"`
	InvalidOutputTokens   int64   `json:"invalidOutputTokens"`
	InvalidOutputCostUSD  float64 `json:"invalidOutputCostUsd"`
	RetryRequests         int64   `json:"retryRequests"`
	RetryTokens           int64   `json:"retryTokens"`
	RetryCostUSD          float64 `json:"retryCostUsd"`
}

// GetAIUsageAnalytics lee solo ai_usage_events: los contadores vienen del objeto
// `usage` del proveedor, no de estimar por longitud del texto.
//
// botID vacío agrega toda la organización.
func GetAIUsageAnalytics(ctx context.Context, pool *pgxpool.Pool, orgID, botID string, from, to time.Time) (*AIUsageAnalytics, error) {
	if !from.Before(to) {
		return nil, fmt.Errorf("el inicio debe ser anterior al fin")
	}
	from, to = from.UTC(), to.UTC()
	out := &AIUsageAnalytics{
		From:     from,
		To:       to,
		Bucket:   usageBucket(from, to),
		Currency: "USD",
		Series:   make([]AIUsageBucket, 0),
		ByNode:   make([]AINodeUsage, 0),
		BySteps:  make([]AIStepsUsage, 0),
		TopChats: make([]AIChatUsage, 0),
	}
	if err := loadAIUsageSeries(ctx, pool, out, orgID, botID, from, to); err != nil {
		return nil, err
	}
	if err := loadAIPerCallStats(ctx, pool, out, orgID, botID, from, to); err != nil {
		return nil, err
	}
	if err := loadAIUsageByNode(ctx, pool, out, orgID, botID, from, to); err != nil {
		return nil, err
	}
	if err := loadAIUsageBySteps(ctx, pool, out, orgID, botID, from, to); err != nil {
		return nil, err
	}
	if err := loadAITopChats(ctx, pool, out, orgID, botID, from, to); err != nil {
		return nil, err
	}
	if err := loadAIWastedUsage(ctx, pool, out, orgID, botID, from, to); err != nil {
		return nil, err
	}
	return out, nil
}

// El truncado se hace sobre la hora UTC explícita, no sobre el timestamptz: de
// lo contrario el corte del día dependería del TimeZone de la sesión de Postgres
// y el mismo periodo daría barras distintas según quién consulte.
func loadAIUsageSeries(ctx context.Context, pool *pgxpool.Pool, out *AIUsageAnalytics, orgID, botID string, from, to time.Time) error {
	rows, err := pool.Query(ctx, `
		SELECT date_trunc($5::text, e.occurred_at AT TIME ZONE 'UTC') AS bucket,
		       COUNT(*)::bigint,
		       COALESCE(SUM(e.input_tokens),0)::bigint,
		       COALESCE(SUM(e.output_tokens),0)::bigint,
		       COALESCE(SUM(e.cache_read_input_tokens),0)::bigint,
		       COALESCE(SUM(e.cache_creation_input_tokens),0)::bigint,
		       COALESCE(SUM(e.estimated_cost_usd),0)::float8,
		       COUNT(DISTINCT e.chat_id)::bigint
		  FROM ai_usage_events e
		 WHERE `+aiUsageFilter+`
		 GROUP BY 1
		 ORDER BY 1`, orgID, botID, from, to, out.Bucket)
	if err != nil {
		return err
	}
	defer rows.Close()

	measured := make([]AIUsageBucket, 0)
	for rows.Next() {
		var item AIUsageBucket
		if err := rows.Scan(&item.StartsAt, &item.Requests, &item.InputTokens, &item.OutputTokens,
			&item.CacheReadInputTokens, &item.CacheCreationInputTokens,
			&item.EstimatedCostUSD, &item.Chats); err != nil {
			return err
		}
		item.StartsAt = item.StartsAt.UTC()
		item.TotalTokens = item.InputTokens + item.OutputTokens +
			item.CacheReadInputTokens + item.CacheCreationInputTokens
		item.EstimatedCostUSD = roundCostUSD(item.EstimatedCostUSD)
		measured = append(measured, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	out.Series = fillSeriesGaps(measured, from, to, out.Bucket)
	return nil
}

// fillSeriesGaps emite un punto por cada bucket del rango, tenga datos o no. La
// clave del mapa es el epoch y no el time.Time: dos instantes iguales con
// distinta Location no son iguales con ==, y el punto medido se perdería.
func fillSeriesGaps(measured []AIUsageBucket, from, to time.Time, bucket string) []AIUsageBucket {
	found := make(map[int64]AIUsageBucket, len(measured))
	for _, item := range measured {
		found[item.StartsAt.UTC().Unix()] = item
	}
	out := make([]AIUsageBucket, 0, len(measured)+1)
	for at := truncateToBucket(from, bucket); at.Before(to); at = nextBucket(at, bucket) {
		if item, ok := found[at.Unix()]; ok {
			out = append(out, item)
			continue
		}
		out = append(out, AIUsageBucket{StartsAt: at})
	}
	return out
}

func loadAIPerCallStats(ctx context.Context, pool *pgxpool.Pool, out *AIUsageAnalytics, orgID, botID string, from, to time.Time) error {
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)::bigint,
		       COALESCE(AVG(total),0)::float8,
		       COALESCE(AVG(input_tokens),0)::float8,
		       COALESCE(AVG(output_tokens),0)::float8,
		       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY total::float8),0)::float8,
		       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY total::float8),0)::float8,
		       COALESCE(MAX(total),0)::bigint,
		       COALESCE(AVG(cost),0)::float8,
		       COALESCE(AVG(duration_ms),0)::float8,
		       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms),0)::float8,
		       COALESCE(SUM(cache_read_input_tokens)::float8
		                / NULLIF(SUM(input_tokens+cache_read_input_tokens+cache_creation_input_tokens),0),0)::float8
		  FROM (
		      SELECT e.input_tokens, e.output_tokens,
		             e.cache_read_input_tokens, e.cache_creation_input_tokens,
		             e.estimated_cost_usd AS cost,
		             `+aiTotalTokensExpr+` AS total,
		             NULLIF(e.metadata->>'duration_ms','')::float8 AS duration_ms
		        FROM ai_usage_events e
		       WHERE `+aiUsageFilter+`
		  ) t`, orgID, botID, from, to).Scan(
		&out.PerCall.Requests, &out.PerCall.AvgTotalTokens, &out.PerCall.AvgInputTokens,
		&out.PerCall.AvgOutputTokens, &out.PerCall.P50TotalTokens, &out.PerCall.P95TotalTokens,
		&out.PerCall.MaxTotalTokens, &out.PerCall.AvgCostUSD, &out.PerCall.AvgDurationMs,
		&out.PerCall.P95DurationMs, &out.PerCall.CacheHitRatio); err != nil {
		return err
	}
	out.PerCall.AvgTotalTokens = roundTokens(out.PerCall.AvgTotalTokens)
	out.PerCall.AvgInputTokens = roundTokens(out.PerCall.AvgInputTokens)
	out.PerCall.AvgOutputTokens = roundTokens(out.PerCall.AvgOutputTokens)
	out.PerCall.P50TotalTokens = roundTokens(out.PerCall.P50TotalTokens)
	out.PerCall.P95TotalTokens = roundTokens(out.PerCall.P95TotalTokens)
	out.PerCall.AvgDurationMs = roundTokens(out.PerCall.AvgDurationMs)
	out.PerCall.P95DurationMs = roundTokens(out.PerCall.P95DurationMs)
	out.PerCall.AvgCostUSD = roundCostUSD(out.PerCall.AvgCostUSD)
	out.PerCall.CacheHitRatio = math.Round(out.PerCall.CacheHitRatio*10_000) / 10_000
	return nil
}

func loadAIUsageByNode(ctx context.Context, pool *pgxpool.Pool, out *AIUsageAnalytics, orgID, botID string, from, to time.Time) error {
	rows, err := pool.Query(ctx, `
		SELECT COALESCE(e.metadata->>'node_id','') AS node_id,
		       COUNT(*)::bigint,
		       COALESCE(SUM(`+aiTotalTokensExpr+`),0)::bigint,
		       COALESCE(AVG(`+aiTotalTokensExpr+`),0)::float8,
		       COALESCE(SUM(e.estimated_cost_usd),0)::float8
		  FROM ai_usage_events e
		 WHERE `+aiUsageFilter+`
		 GROUP BY 1
		 ORDER BY 3 DESC`, orgID, botID, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item AINodeUsage
		if err := rows.Scan(&item.NodeID, &item.Requests, &item.TotalTokens,
			&item.AvgTotalTokens, &item.EstimatedCostUSD); err != nil {
			return err
		}
		item.AvgTotalTokens = roundTokens(item.AvgTotalTokens)
		item.EstimatedCostUSD = roundCostUSD(item.EstimatedCostUSD)
		out.ByNode = append(out.ByNode, item)
	}
	return rows.Err()
}

// Un evento sin `steps` en el metadata es un turno de una sola petición: el
// webhook solo escribe la clave cuando el bucle dio más de una vuelta.
func loadAIUsageBySteps(ctx context.Context, pool *pgxpool.Pool, out *AIUsageAnalytics, orgID, botID string, from, to time.Time) error {
	rows, err := pool.Query(ctx, `
		SELECT COALESCE(NULLIF(e.metadata->>'steps','')::int,1) AS steps,
		       COUNT(*)::bigint,
		       COALESCE(SUM(`+aiTotalTokensExpr+`),0)::bigint,
		       COALESCE(AVG(`+aiTotalTokensExpr+`),0)::float8,
		       COALESCE(SUM(e.estimated_cost_usd),0)::float8
		  FROM ai_usage_events e
		 WHERE `+aiUsageFilter+`
		 GROUP BY 1
		 ORDER BY 1`, orgID, botID, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item AIStepsUsage
		if err := rows.Scan(&item.Steps, &item.Requests, &item.TotalTokens,
			&item.AvgTotalTokens, &item.EstimatedCostUSD); err != nil {
			return err
		}
		item.AvgTotalTokens = roundTokens(item.AvgTotalTokens)
		item.EstimatedCostUSD = roundCostUSD(item.EstimatedCostUSD)
		out.BySteps = append(out.BySteps, item)
	}
	return rows.Err()
}

// El LEFT JOIN es deliberado: chat_id queda en NULL cuando se borra el chat
// (ON DELETE SET NULL), y esas filas se excluyen para no agrupar bajo un mismo
// contacto fantasma conversaciones que no tienen nada que ver.
func loadAITopChats(ctx context.Context, pool *pgxpool.Pool, out *AIUsageAnalytics, orgID, botID string, from, to time.Time) error {
	rows, err := pool.Query(ctx, `
		SELECT e.chat_id::text,
		       COALESCE(c.contact,''),
		       COALESCE(c.contact_name,''),
		       COUNT(*)::bigint,
		       COALESCE(SUM(`+aiTotalTokensExpr+`),0)::bigint,
		       COALESCE(AVG(`+aiTotalTokensExpr+`),0)::float8,
		       COALESCE(SUM(e.estimated_cost_usd),0)::float8,
		       MAX(e.occurred_at)
		  FROM ai_usage_events e
		  LEFT JOIN chats c ON c.id=e.chat_id
		 WHERE `+aiUsageFilter+`
		   AND e.chat_id IS NOT NULL
		 GROUP BY e.chat_id, c.contact, c.contact_name
		 ORDER BY 5 DESC
		 LIMIT $5`, orgID, botID, from, to, topChatsLimit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item AIChatUsage
		if err := rows.Scan(&item.ChatID, &item.Contact, &item.ContactName, &item.Requests,
			&item.TotalTokens, &item.AvgTotalTokens, &item.EstimatedCostUSD, &item.LastAt); err != nil {
			return err
		}
		item.AvgTotalTokens = roundTokens(item.AvgTotalTokens)
		item.EstimatedCostUSD = roundCostUSD(item.EstimatedCostUSD)
		item.LastAt = item.LastAt.UTC()
		out.TopChats = append(out.TopChats, item)
	}
	return rows.Err()
}

func loadAIWastedUsage(ctx context.Context, pool *pgxpool.Pool, out *AIUsageAnalytics, orgID, botID string, from, to time.Time) error {
	const retried = `COALESCE(NULLIF(e.metadata->>'attempt','')::int,1) > 1`
	return pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE e.outcome='invalid_output')::bigint,
		       COALESCE(SUM(`+aiTotalTokensExpr+`) FILTER (WHERE e.outcome='invalid_output'),0)::bigint,
		       COALESCE(SUM(e.estimated_cost_usd) FILTER (WHERE e.outcome='invalid_output'),0)::float8,
		       COUNT(*) FILTER (WHERE `+retried+`)::bigint,
		       COALESCE(SUM(`+aiTotalTokensExpr+`) FILTER (WHERE `+retried+`),0)::bigint,
		       COALESCE(SUM(e.estimated_cost_usd) FILTER (WHERE `+retried+`),0)::float8
		  FROM ai_usage_events e
		 WHERE `+aiUsageFilter, orgID, botID, from, to).Scan(
		&out.Wasted.InvalidOutputRequests, &out.Wasted.InvalidOutputTokens,
		&out.Wasted.InvalidOutputCostUSD, &out.Wasted.RetryRequests,
		&out.Wasted.RetryTokens, &out.Wasted.RetryCostUSD)
}

func usageBucket(from, to time.Time) string {
	switch span := to.Sub(from); {
	case span <= 48*time.Hour:
		return bucketHour
	case span <= 92*24*time.Hour:
		return bucketDay
	default:
		return bucketWeek
	}
}

// truncateToBucket replica date_trunc en UTC. Debe coincidir exactamente con lo
// que devuelve la consulta o el relleno de huecos duplicaría barras en vez de
// completarlas.
func truncateToBucket(at time.Time, bucket string) time.Time {
	at = at.UTC()
	day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	switch bucket {
	case bucketHour:
		return time.Date(at.Year(), at.Month(), at.Day(), at.Hour(), 0, 0, 0, time.UTC)
	case bucketWeek:
		// date_trunc('week') ancla en lunes (ISO); Weekday() cuenta desde domingo.
		return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	default:
		return day
	}
}

func nextBucket(at time.Time, bucket string) time.Time {
	switch bucket {
	case bucketHour:
		return at.Add(time.Hour)
	case bucketWeek:
		return at.AddDate(0, 0, 7)
	default:
		return at.AddDate(0, 0, 1)
	}
}

func roundTokens(value float64) float64 {
	return math.Round(value*10) / 10
}
