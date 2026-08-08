package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Condición de audiencia de un flujo (PLAN-FLUJOS-POR-AUDIENCIA-Y-PERMISOS §2.3).
//
// Un flujo con audiencia solo atiende a los contactos que la cumplen. La
// condición NO es un lenguaje de filtros nuevo: es exactamente la misma
// configuración que el bloque «Leer una tabla» (`data_query`), y por eso este
// validador se apoya en `validateDataQueryArgs` en vez de duplicar sus reglas.
// Si las dos listas se separaran, el panel dibujaría una condición que el
// backend rechaza — el mismo fallo que ya vigila `nodeMeta.ts` para el grafo.
//
// Vive en `engine` porque `validateDataQueryArgs` vive aquí, y `engine` no puede
// importar `models`. La ejecución del predicado sí es de `models`, que es quien
// puede hablar con Postgres.

// AudienceCondition es la condición ya normalizada: el mismo mapa plano de
// argumentos que consume un bloque `data_query`.
type AudienceCondition map[string]string

// AudienceContactsObject nombra la tabla de contactos como objeto del predicado.
//
// Lleva `@` a propósito. `data_objects.key` obliga a `^[a-z][a-z0-9_]{0,62}$`,
// así que ninguna tabla de una organización puede llamarse así jamás. Si el
// centinela fuera `contacts` a secas, una organización que creara su propia
// tabla `contacts` vería cómo el runtime consulta otra cosa **en silencio**: el
// flujo atendería a un conjunto de gente distinto del que su autor configuró, y
// nada en la pantalla lo delataría.
const AudienceContactsObject = "@contacts"

// audienceRejectedArgs son argumentos legítimos en un bloque `data_query` que no
// significan nada en un predicado.
//
// Se rechazan en vez de ignorarse. Un `limit: 50` guardado en una audiencia
// insinúa con fuerza que hace algo —y no hace nada: el predicado solo responde
// sí o no, y se ejecuta con límite 1—. Un argumento que se acepta y se descarta
// en silencio es de donde salen las tardes buscando por qué «el filtro no se
// aplica».
var audienceRejectedArgs = map[string]string{
	"fields":   "un predicado no proyecta campos: solo responde si el contacto entra o no",
	"orderBy":  "ordenar no cambia una respuesta de sí o no",
	"orderDir": "ordenar no cambia una respuesta de sí o no",
	"limit":    "el predicado se ejecuta siempre con límite 1",
}

// ParseAudience normaliza y valida la condición almacenada en `flows.audience`.
//
// Un `raw` vacío, `null` o `{}` significa **sin audiencia**: devuelve (nil, nil)
// y el flujo atiende a todos, que es el comportamiento de siempre.
func ParseAudience(raw json.RawMessage) (AudienceCondition, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return nil, nil
	}
	var args map[string]string
	if err := json.Unmarshal(raw, &args); err != nil {
		// Un `map[string]string` falla si algún valor no es cadena. El mensaje
		// tiene que decirlo, porque el error de encoding/json habla de tipos Go.
		return nil, fmt.Errorf("la condición de audiencia debe ser un objeto de texto a texto")
	}
	if len(args) == 0 {
		return nil, nil
	}
	if err := ValidateAudience(args); err != nil {
		return nil, err
	}
	return args, nil
}

// ValidateAudience impone, sobre las reglas de `data_query`, las tres que solo
// tienen sentido cuando la condición se evalúa **antes** de que exista un grafo.
func ValidateAudience(args map[string]string) error {
	for key, why := range audienceRejectedArgs {
		if strings.TrimSpace(args[key]) != "" {
			return fmt.Errorf("la audiencia no admite %s: %s", key, why)
		}
	}

	// `linkCurrentContact` es lo ÚNICO que ata la condición al contacto de este
	// mensaje, y lo pone el runtime, nunca el autor. Sin él la condición no habla
	// de *este* contacto: sería un interruptor global —«¿existe alguna fila que
	// cumpla?»— disfrazado de audiencia, y respondería lo mismo para todo el
	// mundo. Exigirlo explícitamente y no darlo por supuesto es lo que impide que
	// una condición razonable a la vista abra el flujo a la organización entera.
	//
	// Sobre `@contacts` la implementación es otra —la fila del contacto se busca
	// por teléfono, no por `data_record_contacts`—, pero se sigue exigiendo. La
	// garantía que expresa es la misma en los dos casos: «esta condición habla de
	// este contacto». Aceptarlo ausente solo ahí obligaría a recordar en qué
	// objeto es obligatorio y en cuál no.
	link := strings.TrimSpace(args["linkCurrentContact"])
	if link == "" {
		return fmt.Errorf("la audiencia requiere linkCurrentContact: sin él la condición no habla del contacto de este mensaje")
	}
	if enabled, err := strconv.ParseBool(link); err != nil || !enabled {
		return fmt.Errorf("la audiencia requiere linkCurrentContact = true")
	}

	// Dentro de un grafo, `value` interpola variables del flujo y ahí está toda
	// la utilidad del bloque. El predicado corre en el despacho, antes de que
	// exista estado: no hay nada que interpolar. Permitirlo dejaría el marcador
	// literal dentro de la comparación, la condición no casaría jamás y el flujo
	// quedaría mudo sin un solo error que lo explique.
	for _, key := range sortedKeys(args) {
		if !strings.HasSuffix(key, ".value") {
			continue
		}
		if strings.Contains(args[key], "{") {
			return fmt.Errorf("la audiencia no admite variables en %s: se evalúa antes de que el flujo tenga estado", key)
		}
	}

	// Y por último las reglas comunes: objeto fijo, campos y operadores sin `{`,
	// nada de argumentos inventados. Aquí importan más que dentro de un grafo,
	// porque esta condición decide qué grafo corre.
	if err := validateDataQueryArgs(args); err != nil {
		return fmt.Errorf("condición de audiencia inválida: %w", err)
	}
	return nil
}

// sortedKeys mantiene estable el mensaje de error cuando falla más de una clave.
// El recorrido de un mapa en Go es aleatorio, y un validador que culpa a
// `where.1.value` unas veces y a `where.2.value` otras hace dudar de si el
// arreglo funcionó.
func sortedKeys(args map[string]string) []string {
	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// AudienceSummary describe la condición en prosa, para que el panel y los logs
// digan a quién atiende el flujo sin que nadie tenga que leer JSON.
func (a AudienceCondition) Summary() string {
	if len(a) == 0 {
		return ""
	}
	object := strings.TrimSpace(a["object"])
	if object == AudienceContactsObject {
		object = "contactos"
	}
	var rules []string
	for i := 1; i <= 8; i++ {
		prefix := "where." + strconv.Itoa(i) + "."
		field := strings.TrimSpace(a[prefix+"field"])
		if field == "" {
			continue
		}
		op := strings.TrimSpace(a[prefix+"op"])
		value := strings.TrimSpace(a[prefix+"value"])
		if op == "exists" {
			rules = append(rules, field+" existe")
			continue
		}
		rules = append(rules, field+" "+op+" "+value)
	}
	if len(rules) == 0 {
		return "contactos con un registro en " + object
	}
	return "contactos con un registro en " + object + " donde " + strings.Join(rules, " y ")
}
