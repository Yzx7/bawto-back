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

// ConfigKey es un dato que **fija el autor del flujo** y el modelo no elige.
// Existe para acotar el alcance: un agente comercial busca en el catálogo de
// servicios, no en cualquier tabla de la organización.
type ConfigKey struct {
	Key      string
	Label    string
	Help     string
	Required bool
	// Kind orienta al editor sobre qué control pintar. "data_object" pide la
	// clave de un objeto de datos de la organización.
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
	Config      []ConfigKey
	Effect      Effect
	// ForAgent: el modelo puede llamarla. ForGraph: puede ponerse en un nodo
	// `tool`. No son excluyentes, pero tampoco automáticas: una herramienta sin
	// `Description` decente no debería ofrecerse al modelo.
	ForAgent bool
	ForGraph bool
}

var registry = map[string]Spec{
	"search_data": {
		Name:  "search_data",
		Label: "Buscar en una tabla",
		Help:  "Consulta los registros de un objeto de datos de la organización y devuelve los que coinciden.",
		Description: "Busca información en el catálogo interno de la empresa. Úsala antes de " +
			"afirmar detalles sobre servicios, productos, precios o condiciones: es la fuente " +
			"de verdad y evita inventar. Si no devuelve resultados, dilo en vez de suponer.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type": "string",
					"description": "Palabras a buscar, en español. Usa términos del dominio " +
						"(por ejemplo «tienda online» o «sensores»), no frases completas.",
					"minLength": 1,
				},
			},
			"required":             []string{"query"},
			"additionalProperties": false,
		},
		Config: []ConfigKey{{
			Key:      "object",
			Label:    "Objeto de datos",
			Help:     "Clave técnica del objeto en el que puede buscar. El modelo no puede salirse de él.",
			Required: true,
			Kind:     "data_object",
		}},
		Effect:   EffectRead,
		ForAgent: true,
		ForGraph: false,
	},

	// La del comprobante es anterior al registro y solo la usa el grafo: resuelve
	// sus entradas del mensaje en curso, así que no tiene esquema que ofrecerle a
	// un modelo.
	"record_payment_receipt": {
		Name:     "record_payment_receipt",
		Label:    "Registrar comprobante de pago",
		Help:     "Adjunta la imagen recibida a la factura pendiente del contacto y la pasa a «validación».",
		Effect:   EffectWrite,
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
