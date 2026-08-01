package ai

import (
	"fmt"
	"sort"
	"strings"
)

// ModelPricing ata un modelo a su proveedor y su tarifario. Existe porque el
// costo se calcula multiplicando tokens por tarifas **copiadas en cada fila** de
// ai_usage_events: si un modelo empieza a escribir filas con la tarifa de otro,
// esas filas nacen mal y no se pueden arreglar después, porque el histórico es
// inmutable a propósito. Antes esto dependía de que quien cambiara `AI_MODEL`
// se acordara de cambiar además cuatro variables sueltas.
type ModelPricing struct {
	Provider string
	Rates    Rates
	// Source documenta de dónde salió el precio y cuándo se consultó. Los
	// tarifarios cambian; sin esto no hay forma de saber si están rancios.
	Source string
}

// catalog son los modelos con tarifa conocida. Un modelo que no esté aquí no se
// arranca sin declarar sus precios a mano: es preferible no arrancar a registrar
// consumo con el precio de otro modelo.
//
// La clave se compara sin distinguir mayúsculas porque los nombres vienen de un
// .env escrito a mano.
var catalog = map[string]ModelPricing{
	"minimax-m3": {
		Provider: "minimax",
		// MiniMax no publica tarifa de escritura de caché para M3 y este cliente
		// no crea breakpoints explícitos, así que la escritura queda en cero.
		// La lectura sí aplica: M3 cachea el prefijo por su cuenta y la informa.
		Rates:  Rates{InputPerMillion: 0.30, OutputPerMillion: 1.20, CacheReadPerMillion: 0.06},
		Source: "minimax pay-as-you-go, contexto <=512K, consultado 2026-07-30",
	},
	"deepseek-v4-flash": {
		Provider: "deepseek",
		// DeepSeek cachea el prefijo automáticamente y no cobra por escribir.
		Rates:  Rates{InputPerMillion: 0.14, OutputPerMillion: 0.28, CacheReadPerMillion: 0.0028},
		Source: "api-docs.deepseek.com/quick_start/pricing, consultado 2026-08-01",
	},
	"deepseek-v4-pro": {
		Provider: "deepseek",
		Rates:    Rates{InputPerMillion: 0.435, OutputPerMillion: 0.87, CacheReadPerMillion: 0.003625},
		Source:   "api-docs.deepseek.com/quick_start/pricing, consultado 2026-08-01",
	},
}

// RatesFor devuelve el tarifario de un modelo conocido.
func RatesFor(model string) (ModelPricing, bool) {
	pricing, ok := catalog[strings.ToLower(strings.TrimSpace(model))]
	return pricing, ok
}

// KnownModels lista el catálogo, para poder decirle a quien arranca qué nombres
// hay en vez de solo que el suyo no vale.
func KnownModels() []string {
	out := make([]string, 0, len(catalog))
	for model := range catalog {
		out = append(out, model)
	}
	sort.Strings(out)
	return out
}

// ResolvePricing decide con qué tarifas se va a registrar el consumo.
//
// El override existe para un cambio de precio del proveedor antes de que se
// actualice el catálogo; por eso gana, pero se exige entero: cuatro tarifas o
// ninguna. Media configuración es la forma más fácil de acabar cobrando la
// entrada nueva con la caché vieja sin que nada avise.
//
// Un modelo desconocido sin override **no arranca**. Es deliberado: el modo de
// fallo alternativo es un backend sano registrando meses de consumo con el
// precio de otro modelo, y eso no se puede deshacer.
func ResolvePricing(provider, model string, override *Rates) (ModelPricing, error) {
	pricing, known := RatesFor(model)

	if override != nil {
		resolved := ModelPricing{
			Provider: strings.TrimSpace(provider),
			Rates:    *override,
			Source:   "tarifas explícitas del entorno",
		}
		if resolved.Provider == "" {
			return ModelPricing{}, fmt.Errorf("con tarifas explícitas hay que declarar AI_PROVIDER")
		}
		return resolved, nil
	}

	if !known {
		return ModelPricing{}, fmt.Errorf(
			"modelo %q sin tarifa conocida: declara las cuatro tarifas en el entorno "+
				"(AI_INPUT_USD_PER_MILLION, AI_OUTPUT_USD_PER_MILLION, "+
				"AI_CACHE_READ_USD_PER_MILLION, AI_CACHE_WRITE_USD_PER_MILLION) "+
				"o usa uno del catálogo: %s",
			model, strings.Join(KnownModels(), ", "))
	}

	// El proveedor declarado tiene que coincidir con el del catálogo. Dejarlo en
	// el anterior tras cambiar de modelo mete los dos tarifarios en el mismo cubo:
	// ai_usage_events indexa por (provider, provider_request_id) y todos los
	// reportes agrupan por proveedor, así que la comparación se vuelve ilegible.
	if declared := strings.TrimSpace(provider); declared != "" && declared != pricing.Provider {
		return ModelPricing{}, fmt.Errorf(
			"AI_PROVIDER=%q no concuerda con el modelo %q, que es de %q",
			declared, model, pricing.Provider)
	}
	return pricing, nil
}
