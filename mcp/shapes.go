package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/Yzx7/sacs-chatbots/engine"
)

// Traducción de errores de forma y publicación del esquema de los campos
// estructurados de un nodo.
//
// Los dos problemas son el mismo. Un `tools: ["data_query"]` producía
// «json: cannot unmarshal string into Go struct field Node.nodes.tools of type
// engine.NodeTool»: dice qué está mal, no cuál es la forma correcta, y filtra
// nombres de tipos de Go a un cliente externo. Y `flow_spec` declaraba `tools`
// como `object_list` sin decir qué lleva dentro ese objeto, así que la única
// manera de averiguar la forma era provocar el error.
//
// La solución no enumera campos uno a uno: describe **cualquier** tipo del
// motor por reflexión sobre sus etiquetas JSON. Así `outputFields`, `cases`,
// `args`, `params` y los que se añadan mañana quedan cubiertos sin tocar nada,
// y el esquema publicado no puede desincronizarse del que valida, porque los dos
// salen de la misma struct.

// maxShapeDepth corta la recursión. Ninguna struct del motor anida tanto, pero
// una futura referencia circular colgaría el servicio en vez de fallar.
const maxShapeDepth = 6

// shapeFault convierte el error de una decodificación tipada en algo accionable.
// Si no es un error de forma reconocible, conserva el mensaje original: inventar
// una explicación para un fallo que no se entiende es peor que repetirlo.
func shapeFault(err error) *toolFault {
	var typeError *json.UnmarshalTypeError
	if !errors.As(err, &typeError) {
		// El error viene de authoring envuelto con %w, así que si no es de tipo,
		// tampoco lo será al re-decodificar; se devuelve tal cual.
		return faultf("invalid_document", "el documento no tiene forma de flujo: %v", err)
	}
	field := strings.TrimSpace(typeError.Field)
	if field == "" {
		field = "(raíz)"
	}
	// Go reporta el tipo del **elemento** cuando el error ocurre dentro de una
	// lista: para `tools: ["data_query"]` dice engine.NodeTool, no []NodeTool. Si
	// el campo se reconoce en engine.Node se describe el tipo declarado, que es
	// lo que el cliente tiene que escribir; si no, se cae al que informó Go.
	described, summary := typeError.Type, typeError.Type
	if declared, found := declaredFieldType(lastPathSegment(field)); found {
		described, summary = declared, declared
	}
	expected := describeType(described, 0)
	return &toolFault{
		Code: "invalid_field_shape",
		Message: fmt.Sprintf(
			"el campo %q recibió %s y espera %s. Consulta flow_spec con section=nodes: "+
				"`fieldShapes` trae la forma exacta de cada campo estructurado.",
			field, typeError.Value, shapeSummary(summary)),
		Data: map[string]any{
			"field":    field,
			"received": typeError.Value,
			"expected": expected,
		},
	}
}

// staleChecksumFault es la única respuesta a un checksum que no corresponde al
// borrador vivo, venga de la comprobación previa o del CAS del modelo. Una sola
// forma para las dos porque para quien la lee el remedio es idéntico.
func staleChecksumFault(expected, current string, updatedAt time.Time) *toolFault {
	return &toolFault{
		Code: "draft_conflict",
		Message: "expectedChecksum no corresponde al borrador vivo: no reenvíes esta copia. " +
			"Vuelve a leerlo con flow_get, fusiona tu cambio sobre el estado actual —incluidas " +
			"posiciones y conexiones que haya movido una persona— y reintenta con el checksum nuevo.",
		Data: map[string]any{
			"expectedChecksum": expected,
			"currentChecksum":  current,
			"currentUpdatedAt": updatedAt,
		},
	}
}

// declaredFieldType busca un campo por su nombre JSON en las structs que
// componen el documento. Es lo que permite describir el tipo declarado —la
// lista— y no el elemento suelto que Go menciona al fallar dentro de ella.
//
// Se miran Node y Flow: así queda cubierto tanto `nodes.tools` como un
// `edges: ["x"]` en la raíz, sin una tabla que mantener.
func declaredFieldType(name string) (reflect.Type, bool) {
	if name == "" {
		return nil, false
	}
	for _, owner := range []reflect.Type{reflect.TypeOf(engine.Node{}), reflect.TypeOf(engine.Flow{})} {
		for index := 0; index < owner.NumField(); index++ {
			field := owner.Field(index)
			if jsonFieldName(field) == name {
				return field.Type, true
			}
		}
	}
	return nil, false
}

func lastPathSegment(path string) string {
	if position := strings.LastIndex(path, "."); position >= 0 {
		return path[position+1:]
	}
	return path
}

// nodeFieldShapes describe los campos estructurados de un nodo a partir de
// engine.Node. Se deriva de la struct y no de una tabla escrita a mano: una
// tabla se queda vieja en silencio en cuanto el motor gana un campo, y el
// síntoma sería documentación que miente, que es peor que no tenerla.
func nodeFieldShapes() map[string]any {
	shapes := map[string]any{}
	nodeType := reflect.TypeOf(engine.Node{})
	for index := 0; index < nodeType.NumField(); index++ {
		field := nodeType.Field(index)
		name := jsonFieldName(field)
		if name == "" || !isStructuredKind(field.Type) {
			continue
		}
		shapes[name] = describeType(field.Type, 0)
	}
	return shapes
}

// isStructuredKind selecciona lo que necesita explicación. Un string o un bool
// se explican solos; una lista de objetos o un mapa, no.
func isStructuredKind(fieldType reflect.Type) bool {
	switch fieldType.Kind() {
	case reflect.Struct, reflect.Map:
		return true
	case reflect.Slice, reflect.Array:
		return fieldType.Elem().Kind() == reflect.Struct || fieldType.Elem().Kind() == reflect.Map
	case reflect.Pointer:
		return isStructuredKind(fieldType.Elem())
	default:
		return false
	}
}

// describeType arma un esqueleto JSON del tipo. Los campos obligatorios —los que
// no llevan omitempty— se marcan, porque saber que `ref` no es opcional es la
// mitad de la información que hacía falta.
func describeType(fieldType reflect.Type, depth int) any {
	if fieldType == nil || depth > maxShapeDepth {
		return "cualquier JSON"
	}
	switch fieldType.Kind() {
	case reflect.Pointer:
		return describeType(fieldType.Elem(), depth)
	case reflect.Slice, reflect.Array:
		if fieldType.Elem().Kind() == reflect.Uint8 {
			return "cualquier JSON"
		}
		return []any{describeType(fieldType.Elem(), depth+1)}
	case reflect.Map:
		return map[string]any{"<clave>": describeType(fieldType.Elem(), depth+1)}
	case reflect.Struct:
		shape := map[string]any{}
		for index := 0; index < fieldType.NumField(); index++ {
			field := fieldType.Field(index)
			name := jsonFieldName(field)
			if name == "" {
				continue
			}
			described := describeType(field.Type, depth+1)
			if text, isText := described.(string); isText && !hasOmitEmpty(field) {
				described = text + " (obligatorio)"
			}
			shape[name] = described
		}
		return shape
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number"
	default:
		return "cualquier JSON"
	}
}

// shapeSummary es la versión de una línea para el mensaje de error.
func shapeSummary(fieldType reflect.Type) string {
	if fieldType == nil {
		return "otro valor"
	}
	switch fieldType.Kind() {
	case reflect.Pointer:
		return shapeSummary(fieldType.Elem())
	case reflect.Slice, reflect.Array:
		return "una lista de " + shapeSummary(fieldType.Elem())
	case reflect.Map:
		return "un objeto de clave y valor"
	case reflect.Struct:
		return "objetos"
	case reflect.String:
		return "un texto"
	case reflect.Bool:
		return "un booleano"
	default:
		return "otro valor"
	}
}

func jsonFieldName(field reflect.StructField) string {
	if field.PkgPath != "" {
		return ""
	}
	tag := field.Tag.Get("json")
	if tag == "-" {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return field.Name
	}
	return name
}

func hasOmitEmpty(field reflect.StructField) bool {
	_, options, _ := strings.Cut(field.Tag.Get("json"), ",")
	return strings.Contains(options, "omitempty")
}
