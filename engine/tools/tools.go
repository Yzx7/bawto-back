// Package tools declara **qué** herramientas existen, sin implementarlas.
//
// Está separado a propósito y no importa nada del proyecto: lo leen tres sitios
// que no pueden depender unos de otros. `engine` valida contra él sin tocar la
// base de datos, `engine/ai` construye con él los parámetros que ve el modelo, y
// la capa que sí habla con Postgres registra los ejecutores. Si las
// especificaciones vivieran junto a los ejecutores, `models` —que ya importa
// `engine`— cerraría un ciclo de importación.
//
// Una herramienta tiene dos formas de invocarse y no son intercambiables:
//
//   - Desde el nodo `tool` del grafo: la llama una arista, con argumentos
//     interpolados, y su resultado va a `saveAs`. El grafo decide qué pasa
//     después por las ramas `ok`/`error`.
//   - Desde el nodo `agent`: la llama el modelo cuando le conviene, con
//     argumentos que él redacta, y el resultado vuelve a su contexto. No hay
//     ramas: el modelo lee el resultado y sigue razonando.
//
// Por eso `Description` e `InputSchema` solo le sirven al agente, y `saveAs` y
// las ramas solo al grafo. Cada consumidor ignora la mitad de la ficha.
package tools

import "sort"

// Effect clasifica lo que la herramienta le hace al mundo. Es la base de la
// compuerta: entregarle a un modelo una herramienta de lectura y una que escribe
// en la tabla de pagos no tiene el mismo riesgo, y la diferencia debe ser
// explícita y no quedar al criterio de quien arma el flujo.
type Effect string

const (
	// EffectRead no cambia nada. El peor fallo posible es una respuesta imprecisa.
	EffectRead Effect = "read"
	// EffectWrite modifica datos de la organización.
	EffectWrite Effect = "write"
	// EffectSend le manda algo al cliente o a un tercero, con coste.
	EffectSend Effect = "send"
)

// PayloadType describe el contrato semántico de entrada/salida de una tool.
// No es un MIME type ni el JSON Schema de sus argumentos: sirve para que el
// editor y, más adelante, el validador del grafo puedan detectar conexiones
// imposibles antes de ejecutar el flujo.
type PayloadType string

const (
	PayloadDataQuery      PayloadType = "data_query"
	PayloadDataRecords    PayloadType = "data_records"
	PayloadStructuredData PayloadType = "structured_data"
	PayloadDataRecord     PayloadType = "data_record"
)

// ConfigKey es un dato que **fija el autor del flujo** y el modelo no elige.
// Existe para acotar el alcance: un agente comercial busca en el catálogo de
// servicios, no en cualquier tabla de la organización.
type ConfigKey struct {
	Key      string
	Label    string
	Help     string
	Required bool
	// Kind orienta al editor sobre qué control pintar. "data_object" pide la
	// clave de un objeto de datos de la organización; "connection", la clave de
	// una conexión externa configurada por la organización.
	Kind string
}

// Spec es la ficha de una herramienta.
type Spec struct {
	Name string
	// Label y Help son para el operador, en el panel.
	Label string
	Help  string
	// Description es lo que lee **el modelo** para decidir si la llama. Se
	// escribe pensando en él, no en el operador: dice qué resuelve y cuándo
	// conviene usarla.
	Description string
	// InputSchema es el JSON Schema de lo que el modelo debe rellenar.
	InputSchema map[string]any
	// Accepts/Produces hacen visible el contrato de datos, independientemente de
	// si la invocación ocurre desde el grafo o dentro de un agente.
	Accepts  []PayloadType
	Produces PayloadType
	Config   []ConfigKey
	Effect   Effect
	// ForAgent: el modelo puede llamarla. ForGraph: puede ponerse en un nodo
	// `tool`. No son excluyentes, pero tampoco automáticas: una herramienta sin
	// `Description` decente no debería ofrecerse al modelo.
	ForAgent bool
	ForGraph bool
}

var registry = map[string]Spec{
	"data_query": {
		Name:  "data_query",
		Label: "Leer una tabla",
		Help: "Consulta los registros de un objeto de datos con filtros, orden y límite fijados en el bloque. " +
			"Devuelve `found`, `count`, `first` y la lista completa.",
		Description: "Consulta la información registrada de la empresa. Úsala antes de afirmar " +
			"detalles sobre servicios, productos, precios o condiciones: es la fuente de verdad " +
			"y evita inventar. Si no devuelve resultados, dilo en vez de suponer.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type": "string",
					"description": "Palabras a buscar, en español. Usa términos del dominio " +
						"(por ejemplo «tienda online» o «sensores»), no frases completas.",
				},
				"filters": map[string]any{
					"type": "array",
					"description": "Condiciones exactas sobre campos concretos. Omítelo si no " +
						"conoces el nombre del campo: la búsqueda por texto ya cubre el caso normal.",
					"maxItems": 4,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"field": map[string]any{"type": "string", "description": "Clave del campo."},
							"op": map[string]any{
								"type":        "string",
								"enum":        []string{"eq", "contains", "in"},
								"description": "`in` compara contra una lista separada por comas.",
							},
							"value": map[string]any{"type": "string"},
						},
						"required":             []string{"field", "op", "value"},
						"additionalProperties": false,
					},
				},
			},
			"additionalProperties": false,
		},
		Accepts:  []PayloadType{PayloadDataQuery},
		Produces: PayloadDataRecords,
		Config: []ConfigKey{
			{
				Key:      "object",
				Label:    "Objeto de datos",
				Help:     "Clave técnica del objeto en el que puede buscar. El modelo no puede salirse de él.",
				Required: false,
				Kind:     "data_object",
			},
			{
				Key:   "objects",
				Label: "Objetos de datos",
				Help:  "Claves fijas separadas por coma. Úsalo cuando un único agente consulta más de un catálogo.",
				Kind:  "text",
			},
			{
				Key:   "fields",
				Label: "Campos que puede ver",
				Help:  "Claves separadas por coma. Vacío devuelve todos los campos del objeto.",
				Kind:  "text",
			},
			{
				Key:   "filterFields",
				Label: "Campos por los que puede filtrar",
				Help: "Claves separadas por coma. Vacío deja al modelo solo la búsqueda por texto; " +
					"nunca puede filtrar por un campo que no esté aquí.",
				Kind: "text",
			},
			{
				Key:   "maxLimit",
				Label: "Máximo de registros",
				Help:  "Tope de resultados que vuelven al modelo. Por defecto 8.",
				Kind:  "text",
			},
		},
		Effect:   EffectRead,
		ForAgent: true,
		ForGraph: true,
	},

	// catalog_search y catalog_product consultan la tienda **en el momento**, no
	// una copia. Por eso su rama `error` importa más que en una lectura local: el
	// flujo tiene que poder distinguir «no lo vendemos» de «la tienda no
	// responde», y sin esa diferencia el bot acabaría afirmando que algo no
	// existe cada vez que se cae una API.
	"catalog_search": {
		Name:  "catalog_search",
		Label: "Buscar en el catálogo",
		Help: "Consulta el catálogo de una tienda conectada, en directo. El bloque fija la conexión, la categoría y el tope de resultados. " +
			"Devuelve `found`, `count`, `first` y la lista completa.",
		Description: "Consulta el catálogo real de la tienda: qué productos existen, su precio, " +
			"su stock y su enlace. Úsala **antes** de afirmar que algo se vende, cuánto cuesta o " +
			"si queda disponible: es la fuente de verdad y evita inventar. Si no devuelve " +
			"resultados, dilo claramente en vez de suponer que existe.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type": "string",
					"description": "Palabras a buscar en el nombre y la descripción, en español. " +
						"Usa términos del producto («sensor de movimiento», «ESP32»), no la frase " +
						"completa del cliente.",
				},
				"minPrice": map[string]any{
					"type":        "number",
					"description": "Precio mínimo. Omítelo salvo que el cliente diera un rango.",
				},
				"maxPrice": map[string]any{
					"type":        "number",
					"description": "Precio máximo. Útil cuando el cliente pide algo «hasta X».",
				},
				"sort": map[string]any{
					"type":        "string",
					"enum":        []string{"price-asc", "price-desc", "name-asc", "newest"},
					"description": "Orden. Usa price-asc cuando pidan lo más barato.",
				},
			},
			"additionalProperties": false,
		},
		Accepts:  []PayloadType{PayloadDataQuery},
		Produces: PayloadDataRecords,
		Config: []ConfigKey{
			{
				Key:      "connection",
				Label:    "Conexión",
				Help:     "Clave técnica de la conexión externa de la organización. El modelo no puede cambiarla.",
				Required: true,
				Kind:     "connection",
			},
			{
				Key:   "categoryId",
				Label: "Categoría",
				Help:  "ID de categoría de la tienda. Acota la búsqueda; vacío busca en todo el catálogo.",
				Kind:  "text",
			},
			{
				Key:   "includeDescendants",
				Label: "Incluir subcategorías",
				Help:  "true incluye la rama completa bajo esa categoría.",
				Kind:  "text",
			},
			{
				Key:   "maxLimit",
				Label: "Máximo de productos",
				Help:  "Tope de resultados. Por defecto 5; el máximo admitido es 10.",
				Kind:  "text",
			},
			{
				Key:   "urlTemplate",
				Label: "Plantilla del enlace",
				Help: "Cómo se arma el enlace público, por ejemplo https://tutienda.com/productos/{slug}. " +
					"Vacío deja `url` sin rellenar: la API no conoce las rutas del storefront.",
				Kind: "text",
			},
		},
		Effect:   EffectRead,
		ForAgent: true,
		ForGraph: true,
	},

	"catalog_product": {
		Name:  "catalog_product",
		Label: "Ver un producto",
		Help: "Trae el detalle de un producto de la tienda conectada: descripción, especificaciones e imágenes. " +
			"Existe aparte de la búsqueda porque la lista de la API no incluye esos campos.",
		Description: "Trae el detalle completo de un producto concreto: descripción larga, " +
			"especificaciones técnicas e imágenes. Úsala cuando ya sabes de qué producto se habla " +
			"—porque la búsqueda te dio su id— y necesitas más detalle del que dio la lista.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"productId": map[string]any{
					"type":        "integer",
					"description": "ID del producto, tal como lo devolvió la búsqueda. No lo inventes.",
				},
				"slug": map[string]any{
					"type":        "string",
					"description": "Alternativa al id, si es lo que conoces.",
				},
			},
			"additionalProperties": false,
		},
		Accepts:  []PayloadType{PayloadDataQuery},
		Produces: PayloadDataRecords,
		Config: []ConfigKey{
			{
				Key:      "connection",
				Label:    "Conexión",
				Help:     "Clave técnica de la conexión externa de la organización.",
				Required: true,
				Kind:     "connection",
			},
			{
				Key:   "urlTemplate",
				Label: "Plantilla del enlace",
				Help:  "Igual que en la búsqueda: https://tutienda.com/productos/{slug}.",
				Kind:  "text",
			},
		},
		Effect:   EffectRead,
		ForAgent: true,
		ForGraph: true,
	},

	"data_mutate": {
		Name:  "data_mutate",
		Label: "Guardar en una tabla",
		Help:  "Crea, actualiza o hace upsert de un registro usando únicamente el objeto y campos fijados en el bloque.",
		Accepts: []PayloadType{
			PayloadStructuredData, PayloadDataRecords,
		},
		Produces: PayloadDataRecord,
		Effect:   EffectWrite,
		ForAgent: false,
		ForGraph: true,
	},

	// Las tres herramientas de venta son **solo de grafo**. La escritura no se le
	// entrega al modelo: el agente recopila qué quiere el cliente y el grafo
	// ejecuta, igual que en el circuito de cobros. Un modelo que puede crear
	// pedidos puede crear pedidos por error.
	"order_create": {
		Name:  "order_create",
		Label: "Crear pedido",
		Help: "Crea el pedido en la tienda conectada. El precio, el stock y los totales los calcula la tienda: " +
			"lo que se envíe como precio se ignora. Requiere clave de idempotencia.",
		Accepts:  []PayloadType{PayloadStructuredData, PayloadDataRecords},
		Produces: PayloadDataRecord,
		Effect:   EffectWrite,
		ForAgent: false,
		ForGraph: true,
	},

	"payment_intent_create": {
		Name:  "payment_intent_create",
		Label: "Preparar el pago del pedido",
		Help: "Abre el cobro del pedido y compone el mensaje exacto con las cuentas de la tienda. " +
			"La IA no redacta números de cuenta.",
		Accepts:  []PayloadType{PayloadDataRecord},
		Produces: PayloadStructuredData,
		Effect:   EffectWrite,
		ForAgent: false,
		ForGraph: true,
	},

	"payment_submit": {
		Name:  "payment_submit",
		Label: "Declarar el pago",
		Help: "Registra el número de operación que declaró el comprador. Reenviarlo corrige la declaración " +
			"en vez de duplicarla. Confirmar el pago sigue siendo del dueño de la tienda.",
		Accepts:  []PayloadType{PayloadStructuredData, PayloadDataRecord},
		Produces: PayloadDataRecord,
		Effect:   EffectWrite,
		ForAgent: false,
		ForGraph: true,
	},

	"subscription_activate": {
		Name:  "subscription_activate",
		Label: "Activar suscripción Bawto",
		Help: "Valida el código y el plan contra Data, y asigna acceso a otra organización. " +
			"Sólo funciona en el bot de la organización comercial propietaria del ledger.",
		Accepts:  []PayloadType{PayloadDataRecord, PayloadStructuredData},
		Produces: PayloadDataRecord,
		Effect:   EffectWrite,
		ForAgent: false,
		ForGraph: true,
	},

	"payment_methods_render": {
		Name:  "payment_methods_render",
		Label: "Preparar métodos de pago",
		Help: "Lee todos los métodos de pago Bawto activos en Data, los ordena por prioridad " +
			"y devuelve un único mensaje exacto. La IA no redacta números ni cuentas.",
		Accepts:  []PayloadType{PayloadDataRecords},
		Produces: PayloadStructuredData,
		Effect:   EffectRead,
		ForAgent: false,
		ForGraph: true,
	},
}

// Get devuelve la ficha (nil si no existe).
func Get(name string) *Spec {
	if spec, ok := registry[name]; ok {
		return &spec
	}
	return nil
}

// ForAgent lista las herramientas que un nodo `agent` puede ofrecerle al modelo,
// en orden estable para que el prompt no cambie entre ejecuciones.
func ForAgent() []Spec {
	return filter(func(s Spec) bool { return s.ForAgent })
}

// ForGraph lista las que pueden ponerse en un nodo `tool`.
func ForGraph() []Spec {
	return filter(func(s Spec) bool { return s.ForGraph })
}

func filter(keep func(Spec) bool) []Spec {
	out := make([]Spec, 0, len(registry))
	for _, spec := range registry {
		if keep(spec) {
			out = append(out, spec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
