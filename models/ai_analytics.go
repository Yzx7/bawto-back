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

// Los cortes de día y la hora del día se calculan en hora de Perú, no en UTC:
// la pregunta que contestan —cuándo habla la gente, cuánto cae en la franja de
// precio pico del proveedor— es de reloj de pared local. Perú no aplica horario
// de verano, así que el desfase es fijo y no hace falta la base de zonas
// horarias ni en Postgres ni en Go; usarlo explícito también evita que el corte
// dependa del TimeZone de la sesión de Postgres.
const (
	limaOffset    = -5 * time.Hour
	limaSQLOffset = `INTERVAL '-05:00'`
)

// Los cuatro contadores son consumo real facturable por el proveedor: la
// lectura de caché es más barata que la entrada fresca, pero no es gratis.
const aiTotalTokensExpr = "(e.input_tokens+e.output_tokens+e.cache_read_input_tokens+e.cache_creation_input_tokens)"

const (
	aiDurationExpr = "NULLIF(e.metadata->>'duration_ms','')::float8"
	// Un evento sin `steps` en el metadata es un turno de una sola petición: el
	// webhook solo escribe la clave cuando el bucle dio más de una vuelta.
	aiStepsExpr = "COALESCE(NULLIF(e.metadata->>'steps','')::int,1)"
	// Igual con `attempt`: solo el reintento deja marca.
	aiRetriedExpr = "COALESCE(NULLIF(e.metadata->>'attempt','')::int,1) > 1"
)

// El alcance es idéntico en todas las consultas y todas alias la tabla como `e`.
// Se comparte como constante para que un cambio no se aplique a unas sí y a
// otras no, que daría cifras incoherentes dentro del mismo panel.
const aiUsageScope = `e.organization_id=$1::uuid
	   AND ($2='' OR e.bot_id=$2::uuid)
	   AND e.purpose='flow_runtime'
	   AND e.occurred_at >= $3 AND e.occurred_at < $4`

// aiUsageFilter añade el modelo. La comparación entre modelos (ByModel) usa el
// alcance **sin** este filtro: filtrar por modelo la tabla que existe para
// comparar modelos la dejaría con una sola fila.
const aiUsageFilter = aiUsageScope + `
	   AND ($5='' OR e.model=$5)`

// AIUsageAnalytics es el detalle de consumo de tokens de un periodo. No sustituye
// a CostReport: aquel valoriza la factura, este explica de dónde sale el gasto.
type AIUsageAnalytics struct {
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Bucket   string    `json:"bucket"`
	Currency string    `json:"currency"`
	// Model es el filtro aplicado; vacío significa todos.
	Model    string             `json:"model"`
	Series   []AIUsageBucket    `json:"series"`
	PerCall  AIPerCallStats     `json:"perCall"`
	ByModel  []AIModelBreakdown `json:"byModel"`
	ByHour   []AIHourUsage      `json:"byHour"`
	ByNode   []AINodeUsage      `json:"byNode"`
	BySteps  []AIStepsUsage     `json:"bySteps"`
	TopChats []AIChatUsage      `json:"topChats"`
	Wasted   AIWastedUsage      `json:"wasted"`
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

// AIModelBreakdown es la tabla que decide qué modelo conviene. El orden de
// lectura es puerta → guardarraíl → economía:
//
//   - **Puerta:** ContractOkRatio y RetryRatio. Un modelo que no devuelve la
//     herramienta forzada de forma fiable queda descartado y su precio da igual,
//     porque cada fallo es una conversación rota **y** un cobro doble.
//   - **Guardarraíl:** P95DurationMs. En WhatsApp una respuesta lenta se lee
//     como un bot roto.
//   - **Economía:** CostPerMessageUSD ordena a los que pasaron la puerta.
//
// La calidad de verdad —el acierto de rama— no está aquí: no se puede medir sin
// etiquetas y sale de la evaluación contra el conjunto etiquetado, no de
// producción.
type AIModelBreakdown struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`

	Requests        int64   `json:"requests"`
	OkRequests      int64   `json:"okRequests"`
	InvalidRequests int64   `json:"invalidRequests"`
	ContractOkRatio float64 `json:"contractOkRatio"`
	RetryRequests   int64   `json:"retryRequests"`
	RetryRatio      float64 `json:"retryRatio"`

	// InboundMessages son mensajes entrantes distintos atendidos. Es el
	// denominador honesto: varias peticiones al modelo pueden pertenecer al
	// mismo turno del cliente.
	InboundMessages int64   `json:"inboundMessages"`
	AvgStepsPerTurn float64 `json:"avgStepsPerTurn"`
	AvgTotalTokens  float64 `json:"avgTotalTokens"`
	P95TotalTokens  float64 `json:"p95TotalTokens"`
	CacheHitRatio   float64 `json:"cacheHitRatio"`

	AvgDurationMs float64 `json:"avgDurationMs"`
	P95DurationMs float64 `json:"p95DurationMs"`

	TotalCostUSD float64 `json:"totalCostUsd"`
	// CostPerMessageUSD reparte **todo** el costo —reintentos, pasos del bucle y
	// tokens tirados en salidas inválidas incluidos— entre los mensajes
	// atendidos. Un modelo con el token barato que necesita tres pasos y dos
	// reintentos sale caro aquí, que es justo lo que la factura refleja y lo que
	// el costo por token esconde.
	CostPerMessageUSD float64 `json:"costPerMessageUsd"`
}

// AIHourUsage es el perfil horario en hora de Perú. Existe para dos preguntas:
// cuándo hay que estar disponible, y cuánto del gasto cae en la franja de precio
// pico de un proveedor que la aplique.
type AIHourUsage struct {
	Hour             int     `json:"hour"`
	Requests         int64   `json:"requests"`
	TotalTokens      int64   `json:"totalTokens"`
	EstimatedCostUSD float64 `json:"estimatedCostUsd"`
	// CostShare es la proporción del costo del periodo que cae en esta hora.
	CostShare float64 `json:"costShare"`
}

// AINodeUsage reparte el gasto entre los nodos IA del grafo. Va agrupado también
// por modelo: hoy solo corre uno a la vez, pero el día que cada nodo pueda elegir
// el suyo, el corte útil es modelo × nodo —un modelo barato puede sobrar para
// clasificar la intención y quedarse corto redactando— y así no hay que rehacer
// la consulta. NodeID vacío es una llamada anterior a que el evento guardara el
// nodo de origen.
type AINodeUsage struct {
	NodeID           string  `json:"nodeId"`
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
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
// botID vacío agrega toda la organización. model vacío agrega todos los modelos,
// salvo en ByModel, que siempre los compara todos.
func GetAIUsageAnalytics(ctx context.Context, pool *pgxpool.Pool, orgID, botID, model string, from, to time.Time) (*AIUsageAnalytics, error) {
	if !from.Before(to) {
		return nil, fmt.Errorf("el inicio debe ser anterior al fin")
	}
	from, to = from.UTC(), to.UTC()
	out := &AIUsageAnalytics{
		From:     from,
		To:       to,
		Bucket:   usageBucket(from, to),
		Currency: "USD",
		Model:    model,
		Series:   make([]AIUsageBucket, 0),
		ByModel:  make([]AIModelBreakdown, 0),
		ByHour:   make([]AIHourUsage, 0),
		ByNode:   make([]AINodeUsage, 0),
		BySteps:  make([]AIStepsUsage, 0),
		TopChats: make([]AIChatUsage, 0),
	}
	loaders := []func(context.Context, *pgxpool.Pool, *AIUsageAnalytics, string, string, string, time.Time, time.Time) error{
		loadAIUsageSeries,
		loadAIPerCallStats,
		loadAIUsageByModel,
		loadAIUsageByHour,
		loadAIUsageByNode,
		loadAIUsageBySteps,
		loadAITopChats,
		loadAIWastedUsage,
	}
	for _, load := range loaders {
		if err := load(ctx, pool, out, orgID, botID, model, from, to); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// El truncado se hace en hora de Perú y se devuelve como instante real: el JSON
// lleva un timestamp honesto y quien lo pinte solo tiene que formatearlo en esa
// zona. Devolver el reloj de pared con una `Z` detrás sería mentir en el tipo.
func loadAIUsageSeries(ctx context.Context, pool *pgxpool.Pool, out *AIUsageAnalytics, orgID, botID, model string, from, to time.Time) error {
	rows, err := pool.Query(ctx, `
		SELECT date_trunc($6::text, e.occurred_at AT TIME ZONE `+limaSQLOffset+`) AS bucket,
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
		 ORDER BY 1`, orgID, botID, from, to, model, out.Bucket)
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
		item.StartsAt = fromLimaWall(item.StartsAt)
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

func loadAIPerCallStats(ctx context.Context, pool *pgxpool.Pool, out *AIUsageAnalytics, orgID, botID, model string, from, to time.Time) error {
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
		             `+aiDurationExpr+` AS duration_ms
		        FROM ai_usage_events e
		       WHERE `+aiUsageFilter+`
		  ) t`, orgID, botID, from, to, model).Scan(
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
	out.PerCall.CacheHitRatio = roundRatio(out.PerCall.CacheHitRatio)
	return nil
}

// Deliberadamente sobre aiUsageScope y no sobre aiUsageFilter: es la tabla que
// existe para comparar modelos entre sí.
func loadAIUsageByModel(ctx context.Context, pool *pgxpool.Pool, out *AIUsageAnalytics, orgID, botID, _ string, from, to time.Time) error {
	rows, err := pool.Query(ctx, `
		SELECT e.provider, e.model,
		       COUNT(*)::bigint,
		       COUNT(*) FILTER (WHERE e.outcome='ok')::bigint,
		       COUNT(*) FILTER (WHERE e.outcome='invalid_output')::bigint,
		       COUNT(*) FILTER (WHERE `+aiRetriedExpr+`)::bigint,
		       COUNT(DISTINCT e.inbound_message_id)::bigint,
		       COALESCE(AVG(`+aiStepsExpr+`),0)::float8,
		       COALESCE(AVG(`+aiTotalTokensExpr+`),0)::float8,
		       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY `+aiTotalTokensExpr+`::float8),0)::float8,
		       COALESCE(SUM(e.cache_read_input_tokens)::float8
		                / NULLIF(SUM(e.input_tokens+e.cache_read_input_tokens+e.cache_creation_input_tokens),0),0)::float8,
		       COALESCE(AVG(`+aiDurationExpr+`),0)::float8,
		       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY `+aiDurationExpr+`),0)::float8,
		       COALESCE(SUM(e.estimated_cost_usd),0)::float8
		  FROM ai_usage_events e
		 WHERE `+aiUsageScope+`
		 GROUP BY e.provider, e.model
		 ORDER BY 14 DESC`, orgID, botID, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item AIModelBreakdown
		if err := rows.Scan(&item.Provider, &item.Model, &item.Requests, &item.OkRequests,
			&item.InvalidRequests, &item.RetryRequests, &item.InboundMessages,
			&item.AvgStepsPerTurn, &item.AvgTotalTokens, &item.P95TotalTokens,
			&item.CacheHitRatio, &item.AvgDurationMs, &item.P95DurationMs,
			&item.TotalCostUSD); err != nil {
			return err
		}
		if item.Requests > 0 {
			item.ContractOkRatio = roundRatio(float64(item.OkRequests) / float64(item.Requests))
			item.RetryRatio = roundRatio(float64(item.RetryRequests) / float64(item.Requests))
		}
		// Sin mensajes entrantes atribuidos no hay denominador honesto; se deja
		// en cero antes que inventar uno con el número de peticiones, que daría
		// un costo por mensaje artificialmente bajo.
		if item.InboundMessages > 0 {
			item.CostPerMessageUSD = roundCostUSD(item.TotalCostUSD / float64(item.InboundMessages))
		}
		item.AvgStepsPerTurn = roundTokens(item.AvgStepsPerTurn)
		item.AvgTotalTokens = roundTokens(item.AvgTotalTokens)
		item.P95TotalTokens = roundTokens(item.P95TotalTokens)
		item.AvgDurationMs = roundTokens(item.AvgDurationMs)
		item.P95DurationMs = roundTokens(item.P95DurationMs)
		item.CacheHitRatio = roundRatio(item.CacheHitRatio)
		item.TotalCostUSD = roundCostUSD(item.TotalCostUSD)
		out.ByModel = append(out.ByModel, item)
	}
	return rows.Err()
}

// Las 24 horas salen siempre, con o sin tráfico: un perfil horario con huecos se
// lee como si esas horas no existieran en vez de como que están vacías.
func loadAIUsageByHour(ctx context.Context, pool *pgxpool.Pool, out *AIUsageAnalytics, orgID, botID, model string, from, to time.Time) error {
	rows, err := pool.Query(ctx, `
		SELECT EXTRACT(HOUR FROM e.occurred_at AT TIME ZONE `+limaSQLOffset+`)::int AS hora,
		       COUNT(*)::bigint,
		       COALESCE(SUM(`+aiTotalTokensExpr+`),0)::bigint,
		       COALESCE(SUM(e.estimated_cost_usd),0)::float8
		  FROM ai_usage_events e
		 WHERE `+aiUsageFilter+`
		 GROUP BY 1`, orgID, botID, from, to, model)
	if err != nil {
		return err
	}
	defer rows.Close()

	horas := make(map[int]AIHourUsage, 24)
	var costoTotal float64
	for rows.Next() {
		var item AIHourUsage
		if err := rows.Scan(&item.Hour, &item.Requests, &item.TotalTokens,
			&item.EstimatedCostUSD); err != nil {
			return err
		}
		costoTotal += item.EstimatedCostUSD
		horas[item.Hour] = item
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for hora := 0; hora < 24; hora++ {
		item := horas[hora]
		item.Hour = hora
		if costoTotal > 0 {
			item.CostShare = roundRatio(item.EstimatedCostUSD / costoTotal)
		}
		item.EstimatedCostUSD = roundCostUSD(item.EstimatedCostUSD)
		out.ByHour = append(out.ByHour, item)
	}
	return nil
}

func loadAIUsageByNode(ctx context.Context, pool *pgxpool.Pool, out *AIUsageAnalytics, orgID, botID, model string, from, to time.Time) error {
	rows, err := pool.Query(ctx, `
		SELECT COALESCE(e.metadata->>'node_id','') AS node_id,
		       e.provider, e.model,
		       COUNT(*)::bigint,
		       COALESCE(SUM(`+aiTotalTokensExpr+`),0)::bigint,
		       COALESCE(AVG(`+aiTotalTokensExpr+`),0)::float8,
		       COALESCE(SUM(e.estimated_cost_usd),0)::float8
		  FROM ai_usage_events e
		 WHERE `+aiUsageFilter+`
		 GROUP BY 1, e.provider, e.model
		 ORDER BY 5 DESC`, orgID, botID, from, to, model)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item AINodeUsage
		if err := rows.Scan(&item.NodeID, &item.Provider, &item.Model, &item.Requests,
			&item.TotalTokens, &item.AvgTotalTokens, &item.EstimatedCostUSD); err != nil {
			return err
		}
		item.AvgTotalTokens = roundTokens(item.AvgTotalTokens)
		item.EstimatedCostUSD = roundCostUSD(item.EstimatedCostUSD)
		out.ByNode = append(out.ByNode, item)
	}
	return rows.Err()
}

func loadAIUsageBySteps(ctx context.Context, pool *pgxpool.Pool, out *AIUsageAnalytics, orgID, botID, model string, from, to time.Time) error {
	rows, err := pool.Query(ctx, `
		SELECT `+aiStepsExpr+` AS steps,
		       COUNT(*)::bigint,
		       COALESCE(SUM(`+aiTotalTokensExpr+`),0)::bigint,
		       COALESCE(AVG(`+aiTotalTokensExpr+`),0)::float8,
		       COALESCE(SUM(e.estimated_cost_usd),0)::float8
		  FROM ai_usage_events e
		 WHERE `+aiUsageFilter+`
		 GROUP BY 1
		 ORDER BY 1`, orgID, botID, from, to, model)
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
func loadAITopChats(ctx context.Context, pool *pgxpool.Pool, out *AIUsageAnalytics, orgID, botID, model string, from, to time.Time) error {
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
		 LIMIT $6`, orgID, botID, from, to, model, topChatsLimit)
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

func loadAIWastedUsage(ctx context.Context, pool *pgxpool.Pool, out *AIUsageAnalytics, orgID, botID, model string, from, to time.Time) error {
	return pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE e.outcome='invalid_output')::bigint,
		       COALESCE(SUM(`+aiTotalTokensExpr+`) FILTER (WHERE e.outcome='invalid_output'),0)::bigint,
		       COALESCE(SUM(e.estimated_cost_usd) FILTER (WHERE e.outcome='invalid_output'),0)::float8,
		       COUNT(*) FILTER (WHERE `+aiRetriedExpr+`)::bigint,
		       COALESCE(SUM(`+aiTotalTokensExpr+`) FILTER (WHERE `+aiRetriedExpr+`),0)::bigint,
		       COALESCE(SUM(e.estimated_cost_usd) FILTER (WHERE `+aiRetriedExpr+`),0)::float8
		  FROM ai_usage_events e
		 WHERE `+aiUsageFilter, orgID, botID, from, to, model).Scan(
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

// limaWall devuelve el reloj de pared peruano de un instante, y fromLimaWall
// deshace la conversión. Son inversas exactas porque el desfase es fijo.
func limaWall(at time.Time) time.Time { return at.UTC().Add(limaOffset) }

func fromLimaWall(wall time.Time) time.Time { return wall.Add(-limaOffset).UTC() }

// truncateToBucket replica date_trunc en hora de Perú y devuelve el instante
// real. Debe coincidir exactamente con lo que devuelve la consulta o el relleno
// de huecos duplicaría barras en vez de completarlas.
func truncateToBucket(at time.Time, bucket string) time.Time {
	wall := limaWall(at)
	day := time.Date(wall.Year(), wall.Month(), wall.Day(), 0, 0, 0, 0, time.UTC)
	switch bucket {
	case bucketHour:
		return fromLimaWall(time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), 0, 0, 0, time.UTC))
	case bucketWeek:
		// date_trunc('week') ancla en lunes (ISO); Weekday() cuenta desde domingo.
		return fromLimaWall(day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7)))
	default:
		return fromLimaWall(day)
	}
}

// Los pasos son de duración fija porque Perú no cambia la hora; con horario de
// verano un "día" no siempre serían 24 h y esto haría falta hacerlo en la zona.
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

func roundRatio(value float64) float64 {
	return math.Round(value*10_000) / 10_000
}
