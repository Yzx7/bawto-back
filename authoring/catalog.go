package authoring

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Yzx7/sacs-chatbots/engine/tools"
)

// FieldType is the JSON shape accepted by an editable authoring field.
type FieldType string

const (
	FieldString     FieldType = "string"
	FieldBoolean    FieldType = "boolean"
	FieldInteger    FieldType = "integer"
	FieldStringList FieldType = "string_list"
	FieldObject     FieldType = "object"
	FieldObjectList FieldType = "object_list"
)

// FieldSpec describes one editable node property. DynamicChildren means that
// paths below the field are map keys. For example, args.field.amount addresses
// the literal key "field.amount" inside args.
type FieldSpec struct {
	Path            string    `json:"path"`
	Label           string    `json:"label"`
	Type            FieldType `json:"type"`
	Required        bool      `json:"required,omitempty"`
	DynamicChildren bool      `json:"dynamicChildren,omitempty"`
	AllowedValues   []string  `json:"allowedValues,omitempty"`
	Help            string    `json:"help,omitempty"`
}

// PortMode explains how a node's output ports are obtained.
type PortMode string

const (
	PortsStatic     PortMode = "static"
	PortsStringList PortMode = "string_list"
	PortsObjectIDs  PortMode = "object_ids"
	PortsWaitInput  PortMode = "wait_input"
	PortsAction     PortMode = "action"
)

// OutputPort keeps the visual port id separate from the serialized edge
// handle. Linear nodes expose "out" in the editor but store an empty handle.
type OutputPort struct {
	ID       string `json:"id"`
	Handle   string `json:"handle,omitempty"`
	Required bool   `json:"required"`
}

// PortSpec is declarative metadata for resolving the ports of a node.
type PortSpec struct {
	Mode     PortMode     `json:"mode"`
	Static   []OutputPort `json:"static,omitempty"`
	Path     string       `json:"path,omitempty"`
	ItemID   string       `json:"itemId,omitempty"`
	Append   []OutputPort `json:"append,omitempty"`
	Fallback []OutputPort `json:"fallback,omitempty"`
}

// NodeKindSpec is the backend authoring manifest for one engine node kind.
type NodeKindSpec struct {
	Kind            string         `json:"kind"`
	Label           string         `json:"label"`
	Description     string         `json:"description"`
	AllowedTriggers []string       `json:"allowedTriggers"`
	Fields          []FieldSpec    `json:"fields"`
	Defaults        map[string]any `json:"defaults"`
	Ports           PortSpec       `json:"ports"`
}

var nodeKindCatalog = []NodeKindSpec{
	{
		Kind: "send", Label: "Enviar", Description: "Envía texto o una plantilla aprobada.",
		AllowedTriggers: []string{"message", "schedule"},
		Fields: []FieldSpec{
			{Path: "body", Label: "Mensaje", Type: FieldString},
			{Path: "templateName", Label: "Plantilla", Type: FieldString},
			{Path: "templateLanguage", Label: "Idioma de plantilla", Type: FieldString},
			{Path: "templateParams", Label: "Parámetros de plantilla", Type: FieldStringList},
		},
		Defaults: map[string]any{"body": ""},
		Ports:    PortSpec{Mode: PortsStatic, Static: linearOutput()},
	},
	{
		Kind: "agent", Label: "Agente IA", Description: "Razona, extrae datos y elige una rama declarada.",
		AllowedTriggers: []string{"message"},
		Fields: []FieldSpec{
			{Path: "instruction", Label: "Instrucción", Type: FieldString, Required: true},
			{Path: "outputs", Label: "Ramas", Type: FieldStringList, Required: true},
			{Path: "outputFields", Label: "Datos de salida", Type: FieldObjectList},
			{Path: "agentRole", Label: "Rol", Type: FieldString, AllowedValues: []string{"specialist", "orchestrator"}},
			{Path: "tools", Label: "Herramientas del agente", Type: FieldObjectList},
			{Path: "contextMode", Label: "Contexto", Type: FieldString, AllowedValues: []string{"none", "recent"}},
			{Path: "silent", Label: "Silencioso", Type: FieldBoolean},
			{Path: "replyOn", Label: "Ramas en las que responde", Type: FieldStringList},
			{Path: "accepts", Label: "Entradas admitidas", Type: FieldStringList, AllowedValues: []string{"text", "interactive", "image"}},
			{Path: "saveAs", Label: "Guardar como", Type: FieldString},
		},
		Defaults: map[string]any{
			"instruction": "", "outputs": []any{"ok", "error"},
			"contextMode": "none", "agentRole": "specialist",
		},
		Ports: PortSpec{Mode: PortsStringList, Path: "outputs"},
	},
	{
		Kind: "wait", Label: "Esperar", Description: "Pausa la ejecución hasta el siguiente evento del contacto.",
		AllowedTriggers: []string{"message"},
		Fields: []FieldSpec{
			{Path: "expect", Label: "Tipo legado", Type: FieldString, AllowedValues: []string{"any", "text", "image"}},
			{Path: "accepts", Label: "Entradas legadas", Type: FieldStringList, AllowedValues: []string{
				"text", "image", "audio", "document", "video", "sticker", "location",
				"contacts", "interactive", "order", "reaction", "unsupported",
			}},
			{Path: "saveAs", Label: "Guardar como", Type: FieldString},
			{Path: "timeoutHours", Label: "Horas de espera", Type: FieldInteger},
		},
		Defaults: map[string]any{},
		Ports:    PortSpec{Mode: PortsWaitInput, Path: "accepts", Fallback: linearOutput()},
	},
	{
		Kind: "tool", Label: "Herramienta", Description: "Ejecuta una capacidad registrada del runtime.",
		AllowedTriggers: []string{"message", "schedule"},
		Fields: []FieldSpec{
			{Path: "toolRef", Label: "Herramienta", Type: FieldString, Required: true},
			{Path: "args", Label: "Configuración", Type: FieldObject, DynamicChildren: true},
			{Path: "saveAs", Label: "Guardar resultado como", Type: FieldString},
		},
		Defaults: map[string]any{"toolRef": "data_mutate", "args": map[string]any{}},
		Ports:    PortSpec{Mode: PortsStatic, Static: branchOutputs("ok", "error")},
	},
	{
		Kind: "router", Label: "Router", Description: "Evalúa casos en orden y conserva una salida default.",
		AllowedTriggers: []string{"message", "schedule"},
		Fields: []FieldSpec{
			{Path: "cases", Label: "Casos", Type: FieldObjectList, Required: true},
		},
		Defaults: map[string]any{"cases": []any{map[string]any{
			"id": "caso_1", "label": "Caso 1", "expression": "input.contentType == text",
		}}},
		Ports: PortSpec{
			Mode: PortsObjectIDs, Path: "cases", ItemID: "id",
			Append: []OutputPort{{ID: "default", Handle: "default", Required: true}},
		},
	},
	{
		Kind: "condition", Label: "Condición", Description: "Ramifica por una expresión booleana.",
		AllowedTriggers: []string{"message", "schedule"},
		Fields: []FieldSpec{
			{Path: "expression", Label: "Expresión", Type: FieldString, Required: true},
		},
		Defaults: map[string]any{"expression": ""},
		Ports:    PortSpec{Mode: PortsStatic, Static: branchOutputs("true", "false")},
	},
	{
		Kind: "action", Label: "Acción", Description: "Asigna variables, deriva o termina la ejecución.",
		AllowedTriggers: []string{"message", "schedule"},
		Fields: []FieldSpec{
			{Path: "action", Label: "Acción", Type: FieldString, Required: true, AllowedValues: []string{"set", "handoff", "end"}},
			{Path: "params", Label: "Parámetros", Type: FieldObject, DynamicChildren: true},
		},
		Defaults: map[string]any{"action": "end"},
		Ports:    PortSpec{Mode: PortsAction, Path: "action", Fallback: linearOutput()},
	},
}

func linearOutput() []OutputPort {
	return []OutputPort{{ID: "out", Required: true}}
}

func branchOutputs(ids ...string) []OutputPort {
	ports := make([]OutputPort, 0, len(ids))
	for _, id := range ids {
		ports = append(ports, OutputPort{ID: id, Handle: id, Required: true})
	}
	return ports
}

// NodeCatalog returns a deep copy in stable kind order.
func NodeCatalog() []NodeKindSpec {
	value, err := cloneJSONValue(nodeKindCatalog)
	if err != nil {
		panic(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var result []NodeKindSpec
	if err := json.Unmarshal(raw, &result); err != nil {
		panic(err)
	}
	return result
}

// GetNodeKind returns a detached descriptor for kind.
func GetNodeKind(kind string) (NodeKindSpec, bool) {
	for _, spec := range NodeCatalog() {
		if spec.Kind == kind {
			return spec, true
		}
	}
	return NodeKindSpec{}, false
}

// ResolveOutputPorts evaluates the declarative port manifest against one
// generic node document.
func ResolveOutputPorts(node map[string]any) ([]OutputPort, error) {
	kind, _ := node["kind"].(string)
	spec, exists := GetNodeKind(kind)
	if !exists {
		return nil, fmt.Errorf("tipo de nodo %q desconocido", kind)
	}
	switch spec.Ports.Mode {
	case PortsStatic:
		return append([]OutputPort(nil), spec.Ports.Static...), nil
	case PortsStringList:
		items, err := stringList(node[spec.Ports.Path])
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", kind, spec.Ports.Path, err)
		}
		return branchOutputs(items...), nil
	case PortsObjectIDs:
		items, ok := node[spec.Ports.Path].([]any)
		if !ok {
			return nil, fmt.Errorf("%s.%s debe ser una lista", kind, spec.Ports.Path)
		}
		ports := make([]OutputPort, 0, len(items)+len(spec.Ports.Append))
		for index, item := range items {
			object, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s.%s[%d] debe ser un objeto", kind, spec.Ports.Path, index)
			}
			id, ok := object[spec.Ports.ItemID].(string)
			if !ok || strings.TrimSpace(id) == "" {
				return nil, fmt.Errorf("%s.%s[%d].%s debe ser texto", kind, spec.Ports.Path, index, spec.Ports.ItemID)
			}
			ports = append(ports, OutputPort{ID: id, Handle: id, Required: true})
		}
		return append(ports, spec.Ports.Append...), nil
	case PortsWaitInput:
		items, err := stringList(node[spec.Ports.Path])
		if err != nil && node[spec.Ports.Path] != nil {
			return nil, fmt.Errorf("%s.%s: %w", kind, spec.Ports.Path, err)
		}
		if len(items) > 1 {
			return branchOutputs(items...), nil
		}
		return append([]OutputPort(nil), spec.Ports.Fallback...), nil
	case PortsAction:
		action, _ := node[spec.Ports.Path].(string)
		if action == "end" {
			return nil, nil
		}
		return append([]OutputPort(nil), spec.Ports.Fallback...), nil
	default:
		return nil, fmt.Errorf("modo de puertos %q desconocido", spec.Ports.Mode)
	}
}

func stringList(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	if items, ok := value.([]string); ok {
		return append([]string(nil), items...), nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("debe ser una lista de textos")
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("debe ser una lista de textos")
		}
		result = append(result, text)
	}
	return result, nil
}

// RuntimeToolConfigField is the serializable author-controlled portion of an
// engine/tools specification.
type RuntimeToolConfigField struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Help     string `json:"help,omitempty"`
	Required bool   `json:"required,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

// RuntimeToolSpec is a read-only projection of engine/tools.Spec. It contains
// no executor and therefore cannot run a business capability.
type RuntimeToolSpec struct {
	Name        string                   `json:"name"`
	Label       string                   `json:"label"`
	Help        string                   `json:"help,omitempty"`
	Description string                   `json:"description,omitempty"`
	InputSchema map[string]any           `json:"inputSchema,omitempty"`
	Accepts     []string                 `json:"accepts,omitempty"`
	Produces    string                   `json:"produces,omitempty"`
	Config      []RuntimeToolConfigField `json:"config,omitempty"`
	Effect      string                   `json:"effect"`
	ForAgent    bool                     `json:"forAgent"`
	ForGraph    bool                     `json:"forGraph"`
}

// RuntimeToolCatalog derives every entry from engine/tools and returns it in
// stable name order.
func RuntimeToolCatalog() []RuntimeToolSpec {
	byName := make(map[string]tools.Spec)
	for _, spec := range tools.ForAgent() {
		byName[spec.Name] = spec
	}
	for _, spec := range tools.ForGraph() {
		byName[spec.Name] = spec
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]RuntimeToolSpec, 0, len(names))
	for _, name := range names {
		spec := byName[name]
		projection := RuntimeToolSpec{
			Name: spec.Name, Label: spec.Label, Help: spec.Help,
			Description: spec.Description, Produces: string(spec.Produces),
			Effect: string(spec.Effect), ForAgent: spec.ForAgent, ForGraph: spec.ForGraph,
		}
		for _, accepted := range spec.Accepts {
			projection.Accepts = append(projection.Accepts, string(accepted))
		}
		for _, field := range spec.Config {
			projection.Config = append(projection.Config, RuntimeToolConfigField{
				Key: field.Key, Label: field.Label, Help: field.Help,
				Required: field.Required, Kind: field.Kind,
			})
		}
		if spec.InputSchema != nil {
			clone, err := cloneJSONValue(spec.InputSchema)
			if err != nil {
				panic(err)
			}
			projection.InputSchema = clone.(map[string]any)
		}
		result = append(result, projection)
	}
	return result
}

// GetRuntimeTool reads through the engine registry instead of maintaining an
// authoring copy.
func GetRuntimeTool(name string) (RuntimeToolSpec, bool) {
	for _, spec := range RuntimeToolCatalog() {
		if spec.Name == name {
			return spec, true
		}
	}
	return RuntimeToolSpec{}, false
}

// CatalogHash fingerprints both authoring node metadata and the runtime tool
// projection. It is suitable for anti-drift metadata in proposals.
func CatalogHash() string {
	payload := struct {
		Nodes []NodeKindSpec    `json:"nodes"`
		Tools []RuntimeToolSpec `json:"tools"`
	}{NodeCatalog(), RuntimeToolCatalog()}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func editableField(kind, path string) (FieldSpec, string, bool) {
	spec, exists := GetNodeKind(kind)
	if !exists {
		return FieldSpec{}, "", false
	}
	for _, field := range spec.Fields {
		if path == field.Path {
			return field, "", true
		}
		if field.DynamicChildren && strings.HasPrefix(path, field.Path+".") {
			child := strings.TrimPrefix(path, field.Path+".")
			if child != "" {
				return field, child, true
			}
		}
	}
	return FieldSpec{}, "", false
}

func validateFieldValue(field FieldSpec, child string, value any) error {
	if value == nil {
		return fmt.Errorf("%s no admite null; usa unsetPaths para borrar", field.Path)
	}
	if child != "" {
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s.%s debe ser texto", field.Path, child)
		}
		return nil
	}
	switch field.Type {
	case FieldString:
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s debe ser texto", field.Path)
		}
		if len(field.AllowedValues) > 0 && text != "" && !containsString(field.AllowedValues, text) {
			return fmt.Errorf("%s tiene el valor %q fuera del catálogo", field.Path, text)
		}
	case FieldBoolean:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s debe ser booleano", field.Path)
		}
	case FieldInteger:
		if !isJSONInteger(value) {
			return fmt.Errorf("%s debe ser un entero", field.Path)
		}
	case FieldStringList:
		items, err := stringList(value)
		if err != nil {
			return fmt.Errorf("%s %w", field.Path, err)
		}
		if len(field.AllowedValues) > 0 {
			for _, item := range items {
				if !containsString(field.AllowedValues, item) {
					return fmt.Errorf("%s contiene el valor %q fuera del catálogo", field.Path, item)
				}
			}
		}
	case FieldObject:
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("%s debe ser un objeto", field.Path)
		}
	case FieldObjectList:
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s debe ser una lista de objetos", field.Path)
		}
		for _, item := range items {
			if _, ok := item.(map[string]any); !ok {
				return fmt.Errorf("%s debe ser una lista de objetos", field.Path)
			}
		}
	default:
		return fmt.Errorf("tipo de campo %q desconocido", field.Type)
	}
	return nil
}

func isJSONInteger(value any) bool {
	switch number := value.(type) {
	case json.Number:
		_, err := number.Int64()
		return err == nil
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	case float64:
		return number == float64(int64(number))
	default:
		return false
	}
}

func containsString(items []string, wanted string) bool {
	for _, item := range items {
		if item == wanted {
			return true
		}
	}
	return false
}
