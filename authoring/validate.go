package authoring

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type DiagnosticSeverity string
type DiagnosticSource string

const (
	SeverityError   DiagnosticSeverity = "error"
	SeverityWarning DiagnosticSeverity = "warning"

	SourceEngine  DiagnosticSource = "engine"
	SourceBinding DiagnosticSource = "binding"
	SourceLint    DiagnosticSource = "lint"
)

// Diagnostic is shared by deterministic engine, binding and quality checks.
type Diagnostic struct {
	Severity DiagnosticSeverity `json:"severity"`
	Source   DiagnosticSource   `json:"source"`
	Code     string             `json:"code"`
	NodeID   string             `json:"nodeId,omitempty"`
	Path     string             `json:"path,omitempty"`
	Message  string             `json:"message"`
}

type DataFieldResource struct {
	Key      string `json:"key"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
}

type DataObjectResource struct {
	Key    string              `json:"key"`
	Fields []DataFieldResource `json:"fields"`
}

// ConnectionResource contains only authoring-safe metadata. ToolRefs is the
// exact set of runtime tools the connection supports.
type ConnectionResource struct {
	Key          string   `json:"key"`
	Driver       string   `json:"driver,omitempty"`
	Status       string   `json:"status"`
	Capabilities []string `json:"capabilities,omitempty"`
	ToolRefs     []string `json:"toolRefs,omitempty"`
}

type TemplateResource struct {
	Name     string `json:"name"`
	Language string `json:"language"`
	Status   string `json:"status"`
}

type VariableResource struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

type ContactFieldResource struct {
	Key   string `json:"key"`
	Label string `json:"label,omitempty"`
	Type  string `json:"type,omitempty"`
}

// AuthoringResourceSnapshot is deliberately detached from models and contains
// schemas, never table rows, credentials or private endpoints.
type AuthoringResourceSnapshot struct {
	DataObjects []DataObjectResource `json:"dataObjects,omitempty"`
	Connections []ConnectionResource `json:"connections,omitempty"`
	Templates   []TemplateResource   `json:"templates,omitempty"`
	Variables   []VariableResource   `json:"variables,omitempty"`
	ContactFields []ContactFieldResource `json:"contactFields,omitempty"`
}

type VariableAvailabilityState string

const (
	VariableDefined      VariableAvailabilityState = "defined"
	VariableMaybeDefined VariableAvailabilityState = "maybe_defined"
)

type VariableAvailability struct {
	Name      string                    `json:"name"`
	State     VariableAvailabilityState `json:"state"`
	Producers []string                  `json:"producers,omitempty"`
}

type AuthoringValidationReport struct {
	Diagnostics []Diagnostic `json:"diagnostics"`
}

func (report AuthoringValidationReport) HasErrors() bool {
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Severity == SeverityError {
			return true
		}
	}
	return false
}

// ValidateForAuthoring combines the runtime authority, tenant bindings and
// non-blocking lint without reimplementing engine.Validate.
func ValidateForAuthoring(raw []byte, resources AuthoringResourceSnapshot) AuthoringValidationReport {
	diagnostics := make([]Diagnostic, 0)
	if err := ValidateCandidate(raw); err != nil {
		diagnostics = append(diagnostics, Diagnostic{
			Severity: SeverityError, Source: SourceEngine, Code: "engine_invalid", Message: err.Error(),
		})
	}
	diagnostics = append(diagnostics, ValidateAuthoringContext(raw, resources)...)
	diagnostics = append(diagnostics, LintCandidate(raw)...)
	sortDiagnostics(diagnostics)
	return AuthoringValidationReport{Diagnostics: diagnostics}
}

// ValidateAuthoringContext validates only references that require a tenant
// resource snapshot. It performs no database access.
func ValidateAuthoringContext(raw []byte, resources AuthoringResourceSnapshot) []Diagnostic {
	document, err := ParseDocument(raw)
	if err != nil {
		return []Diagnostic{{Severity: SeverityError, Source: SourceBinding, Code: "invalid_document", Message: err.Error()}}
	}
	nodes, err := objectArrayField(document, "nodes")
	if err != nil {
		return []Diagnostic{{Severity: SeverityError, Source: SourceBinding, Code: "invalid_document", Message: err.Error()}}
	}
	edges, err := objectArrayField(document, "edges")
	if err != nil {
		return []Diagnostic{{Severity: SeverityError, Source: SourceBinding, Code: "invalid_document", Message: err.Error()}}
	}
	index := newResourceIndex(resources)
	diagnostics := make([]Diagnostic, 0)
	for _, node := range nodes {
		nodeID, _ := node["id"].(string)
		kind, _ := node["kind"].(string)
		switch kind {
		case "send":
			diagnostics = append(diagnostics, validateTemplateBinding(nodeID, node, index)...)
		case "tool":
			diagnostics = append(diagnostics, validateGraphToolBindings(nodeID, node, index)...)
		case "agent":
			diagnostics = append(diagnostics, validateAgentToolBindings(nodeID, node, index)...)
		}
	}
	diagnostics = append(diagnostics, validateVariableBindings(nodes, edges, index.variables)...)
	sortDiagnostics(diagnostics)
	return diagnostics
}

// InspectVariablesAtNode returns the variables available immediately before a
// node. It shares the same data-flow analysis used by binding validation.
func InspectVariablesAtNode(raw []byte, nodeID string, resources AuthoringResourceSnapshot) ([]VariableAvailability, error) {
	document, err := ParseDocument(raw)
	if err != nil {
		return nil, err
	}
	nodes, err := objectArrayField(document, "nodes")
	if err != nil {
		return nil, err
	}
	edges, err := objectArrayField(document, "edges")
	if err != nil {
		return nil, err
	}
	if _, exists := findByID(nodes, nodeID); !exists {
		return nil, fmt.Errorf("el nodo %q no existe", nodeID)
	}
	builtins := defaultVariableSet(resources.Variables)
	universe := cloneBoolSet(builtins)
	producedByNode := make(map[string][]string, len(nodes))
	producers := make(map[string][]string)
	for _, node := range nodes {
		id, _ := node["id"].(string)
		producedByNode[id] = nodeProducedVariables(node)
		for _, variable := range producedByNode[id] {
			root := variableRoot(variable)
			universe[root] = true
			producers[root] = append(producers[root], id)
		}
	}
	definite, maybe := variableDataflow(nodes, edges, builtins, universe, producedByNode)
	names := make([]string, 0, len(universe))
	for name := range universe {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]VariableAvailability, 0, len(names))
	for _, name := range names {
		state := VariableMaybeDefined
		if builtins[name] || definite[nodeID][name] {
			state = VariableDefined
		} else if !maybe[nodeID][name] {
			continue
		}
		producerIDs := append([]string(nil), producers[name]...)
		sort.Strings(producerIDs)
		result = append(result, VariableAvailability{Name: name, State: state, Producers: producerIDs})
	}
	return result, nil
}

type resourceIndex struct {
	objects     map[string]map[string]DataFieldResource
	connections map[string]ConnectionResource
	templates   map[string]TemplateResource
	variables   map[string]bool
}

func newResourceIndex(resources AuthoringResourceSnapshot) resourceIndex {
	index := resourceIndex{
		objects:     make(map[string]map[string]DataFieldResource, len(resources.DataObjects)),
		connections: make(map[string]ConnectionResource, len(resources.Connections)),
		templates:   make(map[string]TemplateResource, len(resources.Templates)),
		variables:   make(map[string]bool, len(resources.Variables)),
	}
	for _, object := range resources.DataObjects {
		fields := make(map[string]DataFieldResource, len(object.Fields))
		for _, field := range object.Fields {
			fields[field.Key] = field
		}
		index.objects[object.Key] = fields
	}
	for _, connection := range resources.Connections {
		index.connections[connection.Key] = connection
	}
	for _, template := range resources.Templates {
		index.templates[template.Name+"\x00"+template.Language] = template
	}
	for _, variable := range resources.Variables {
		index.variables[variable.Name] = true
	}
	return index
}

func validateTemplateBinding(nodeID string, node map[string]any, index resourceIndex) []Diagnostic {
	name, _ := node["templateName"].(string)
	if strings.TrimSpace(name) == "" {
		return nil
	}
	language, _ := node["templateLanguage"].(string)
	template, exists := index.templates[name+"\x00"+language]
	if !exists {
		return []Diagnostic{bindingError("unknown_template", nodeID, "templateName", fmt.Sprintf("la plantilla %s (%s) no existe en el snapshot", name, language))}
	}
	if template.Status != "approved" && template.Status != "active" {
		return []Diagnostic{bindingError("template_not_approved", nodeID, "templateName", fmt.Sprintf("la plantilla %s está en estado %s", name, template.Status))}
	}
	return nil
}

func validateGraphToolBindings(nodeID string, node map[string]any, index resourceIndex) []Diagnostic {
	toolRef, _ := node["toolRef"].(string)
	args := stringObject(node["args"])
	switch toolRef {
	case "data_query", "data_mutate":
		return validateDataObjectBinding(nodeID, toolRef, args, index)
	case "catalog_search", "catalog_product", "order_create", "payment_intent_create", "payment_submit":
		return validateConnectionBinding(nodeID, toolRef, args["connection"], index)
	default:
		return nil
	}
}

func validateAgentToolBindings(nodeID string, node map[string]any, index resourceIndex) []Diagnostic {
	items, _ := node["tools"].([]any)
	diagnostics := make([]Diagnostic, 0)
	for toolIndex, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		toolRef, _ := tool["ref"].(string)
		config := stringObject(tool["config"])
		path := fmt.Sprintf("tools[%d].config", toolIndex)
		switch toolRef {
		case "data_query":
			objects := make([]string, 0)
			if object := strings.TrimSpace(config["object"]); object != "" {
				objects = append(objects, object)
			}
			for _, object := range strings.Split(config["objects"], ",") {
				if object = strings.TrimSpace(object); object != "" {
					objects = append(objects, object)
				}
			}
			for _, object := range objects {
				fields, exists := index.objects[object]
				if !exists {
					diagnostics = append(diagnostics, bindingError("unknown_data_object", nodeID, path, fmt.Sprintf("el objeto %q no existe", object)))
					continue
				}
				if len(objects) == 1 {
					for _, field := range splitCSV(config["fields"] + "," + config["filterFields"]) {
						if _, exists := fields[field]; !exists {
							diagnostics = append(diagnostics, bindingError("unknown_data_field", nodeID, path, fmt.Sprintf("el objeto %s no tiene el campo %s", object, field)))
						}
					}
				}
			}
		case "catalog_search", "catalog_product":
			diagnostics = append(diagnostics, validateConnectionBinding(nodeID, toolRef, config["connection"], index)...)
		}
	}
	return diagnostics
}

func validateDataObjectBinding(nodeID, toolRef string, args map[string]string, index resourceIndex) []Diagnostic {
	objectKey := strings.TrimSpace(args["object"])
	fields, exists := index.objects[objectKey]
	if !exists {
		return []Diagnostic{bindingError("unknown_data_object", nodeID, "args.object", fmt.Sprintf("el objeto %q no existe", objectKey))}
	}
	diagnostics := make([]Diagnostic, 0)
	referenced := make(map[string]string)
	if toolRef == "data_mutate" {
		for key, value := range args {
			if strings.HasPrefix(key, "field.") {
				referenced[strings.TrimPrefix(key, "field.")] = value
			}
		}
		if matchField := strings.TrimSpace(args["matchField"]); matchField != "" {
			referenced[matchField] = args["matchValue"]
		}
		if args["operation"] == "create" || args["operation"] == "upsert" {
			for fieldKey, field := range fields {
				if field.Required {
					if _, supplied := referenced[fieldKey]; !supplied {
						diagnostics = append(diagnostics, bindingError("missing_required_data_field", nodeID, "args.field."+fieldKey, fmt.Sprintf("el campo obligatorio %s.%s no recibe un valor", objectKey, fieldKey)))
					}
				}
			}
		}
	} else {
		for _, field := range splitCSV(args["fields"]) {
			referenced[field] = ""
		}
		if orderBy := strings.TrimSpace(args["orderBy"]); orderBy != "" {
			referenced[orderBy] = ""
		}
		for key, value := range args {
			if strings.HasSuffix(key, ".field") && strings.HasPrefix(key, "where.") {
				referenced[strings.TrimSpace(value)] = ""
			}
		}
	}
	for fieldKey, value := range referenced {
		field, exists := fields[fieldKey]
		if !exists {
			diagnostics = append(diagnostics, bindingError("unknown_data_field", nodeID, "args.field."+fieldKey, fmt.Sprintf("el objeto %s no tiene el campo %s", objectKey, fieldKey)))
			continue
		}
		if value != "" && !strings.Contains(value, "{") && !literalMatchesField(value, field.Type) {
			diagnostics = append(diagnostics, bindingError("data_field_type_mismatch", nodeID, "args.field."+fieldKey, fmt.Sprintf("el literal %q no coincide con el tipo %s de %s.%s", value, field.Type, objectKey, fieldKey)))
		}
	}
	return diagnostics
}

func validateConnectionBinding(nodeID, toolRef, connectionKey string, index resourceIndex) []Diagnostic {
	connectionKey = strings.TrimSpace(connectionKey)
	connection, exists := index.connections[connectionKey]
	if !exists {
		return []Diagnostic{bindingError("unknown_connection", nodeID, "args.connection", fmt.Sprintf("la conexión %q no existe", connectionKey))}
	}
	if connection.Status != "active" && connection.Status != "healthy" {
		return []Diagnostic{bindingError("connection_inactive", nodeID, "args.connection", fmt.Sprintf("la conexión %s está en estado %s", connectionKey, connection.Status))}
	}
	if len(connection.ToolRefs) > 0 && !containsString(connection.ToolRefs, toolRef) {
		return []Diagnostic{bindingError("connection_capability_missing", nodeID, "args.connection", fmt.Sprintf("la conexión %s no declara capacidad para %s", connectionKey, toolRef))}
	}
	return nil
}

func bindingError(code, nodeID, path, message string) Diagnostic {
	return Diagnostic{Severity: SeverityError, Source: SourceBinding, Code: code, NodeID: nodeID, Path: path, Message: message}
}

var placeholderPattern = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_.]*)\}`)

func validateVariableBindings(nodes, edges []map[string]any, resourceVariables map[string]bool) []Diagnostic {
	builtins := defaultVariableSetFromMap(resourceVariables)
	producedByNode := make(map[string][]string, len(nodes))
	producerCount := make(map[string]int)
	allVariables := cloneBoolSet(builtins)
	for _, node := range nodes {
		nodeID, _ := node["id"].(string)
		produced := nodeProducedVariables(node)
		producedByNode[nodeID] = produced
		for _, variable := range produced {
			root := variableRoot(variable)
			producerCount[root]++
			allVariables[root] = true
		}
	}
	diagnostics := make([]Diagnostic, 0)
	for variable, count := range producerCount {
		if count > 1 {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: SeverityError, Source: SourceBinding, Code: "save_as_collision", Path: variable,
				Message: fmt.Sprintf("la variable %s tiene %d productores", variable, count),
			})
		}
	}
	inDefinite, inMaybe := variableDataflow(nodes, edges, builtins, allVariables, producedByNode)
	for _, node := range nodes {
		nodeID, _ := node["id"].(string)
		for _, reference := range nodePlaceholderReferences(node) {
			root := variableRoot(reference.Name)
			if reference.Exempt || builtins[root] || inDefinite[nodeID][root] {
				continue
			}
			if inMaybe[nodeID][root] {
				diagnostics = append(diagnostics, Diagnostic{
					Severity: SeverityWarning, Source: SourceBinding, Code: "maybe_undefined_variable",
					NodeID: nodeID, Path: reference.Path,
					Message: fmt.Sprintf("la variable %s solo está definida en algunos recorridos; protégela con empty(...) o una rama", root),
				})
				continue
			}
			diagnostics = append(diagnostics, bindingError("undefined_variable", nodeID, reference.Path, fmt.Sprintf("la variable %s nunca está definida antes de este nodo", root)))
		}
	}
	return diagnostics
}

func defaultVariableSet(variables []VariableResource) map[string]bool {
	set := map[string]bool{
		"input": true, "input_type": true, "input_wa_id": true,
		"contact_id": true, "contact_name": true, "contact_phone": true,
	}
	for _, variable := range variables {
		set[variableRoot(variable.Name)] = true
	}
	return set
}

func defaultVariableSetFromMap(variables map[string]bool) map[string]bool {
	set := defaultVariableSet(nil)
	for variable := range variables {
		set[variableRoot(variable)] = true
	}
	return set
}

type placeholderReference struct {
	Name   string
	Path   string
	Exempt bool
}

func nodePlaceholderReferences(node map[string]any) []placeholderReference {
	result := make([]placeholderReference, 0)
	var walk func(any, string)
	walk = func(value any, path string) {
		switch typed := value.(type) {
		case string:
			for _, match := range placeholderPattern.FindAllStringSubmatch(typed, -1) {
				exempt := path == "args.urlTemplate" && (match[1] == "slug" || match[1] == "id")
				result = append(result, placeholderReference{Name: match[1], Path: path, Exempt: exempt})
			}
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for key := range typed {
				if path == "" && (key == "id" || key == "kind" || key == "pos") {
					continue
				}
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				childPath := key
				if path != "" {
					childPath = path + "." + key
				}
				walk(typed[key], childPath)
			}
		case []any:
			for index, item := range typed {
				walk(item, fmt.Sprintf("%s[%d]", path, index))
			}
		}
	}
	walk(node, "")
	return result
}

func nodeProducedVariables(node map[string]any) []string {
	kind, _ := node["kind"].(string)
	produced := make([]string, 0)
	if kind == "wait" || kind == "agent" || kind == "tool" {
		if saveAs, ok := node["saveAs"].(string); ok && strings.TrimSpace(saveAs) != "" {
			produced = append(produced, saveAs)
		}
	}
	if kind == "action" && node["action"] == "set" {
		if params, ok := node["params"].(map[string]any); ok {
			for key := range params {
				produced = append(produced, key)
			}
		}
	}
	return produced
}

func variableDataflow(nodes, edges []map[string]any, builtins, universe map[string]bool, produced map[string][]string) (map[string]map[string]bool, map[string]map[string]bool) {
	pred := make(map[string][]string, len(nodes))
	for _, edge := range edges {
		source, _ := edge["source"].(string)
		target, _ := edge["target"].(string)
		if target != "" {
			pred[target] = append(pred[target], source)
		}
	}
	definite := make(map[string]map[string]bool, len(nodes))
	maybe := make(map[string]map[string]bool, len(nodes))
	outDefinite := make(map[string]map[string]bool, len(nodes))
	outMaybe := make(map[string]map[string]bool, len(nodes))
	for _, node := range nodes {
		id, _ := node["id"].(string)
		definite[id] = cloneBoolSet(universe)
		maybe[id] = map[string]bool{}
		outDefinite[id] = cloneBoolSet(universe)
		outMaybe[id] = map[string]bool{}
	}
	for iteration := 0; iteration < len(nodes)*4+4; iteration++ {
		changed := false
		for _, node := range nodes {
			id, _ := node["id"].(string)
			incomingDefinite := map[string]bool(nil)
			incomingMaybe := map[string]bool{}
			for _, predecessor := range pred[id] {
				var predecessorDefinite, predecessorMaybe map[string]bool
				if predecessor == "trigger" {
					predecessorDefinite = builtins
					predecessorMaybe = builtins
				} else {
					predecessorDefinite = outDefinite[predecessor]
					predecessorMaybe = outMaybe[predecessor]
				}
				if incomingDefinite == nil {
					incomingDefinite = cloneBoolSet(predecessorDefinite)
				} else {
					incomingDefinite = intersectBoolSets(incomingDefinite, predecessorDefinite)
				}
				incomingMaybe = unionBoolSets(incomingMaybe, predecessorMaybe)
			}
			if incomingDefinite == nil {
				incomingDefinite = cloneBoolSet(builtins)
				incomingMaybe = cloneBoolSet(builtins)
			}
			newOutDefinite := cloneBoolSet(incomingDefinite)
			newOutMaybe := cloneBoolSet(incomingMaybe)
			for _, variable := range produced[id] {
				root := variableRoot(variable)
				newOutDefinite[root] = true
				newOutMaybe[root] = true
			}
			if !equalBoolSets(definite[id], incomingDefinite) || !equalBoolSets(maybe[id], incomingMaybe) ||
				!equalBoolSets(outDefinite[id], newOutDefinite) || !equalBoolSets(outMaybe[id], newOutMaybe) {
				changed = true
				definite[id], maybe[id] = incomingDefinite, incomingMaybe
				outDefinite[id], outMaybe[id] = newOutDefinite, newOutMaybe
			}
		}
		if !changed {
			break
		}
	}
	return definite, maybe
}

func stringObject(value any) map[string]string {
	object, _ := value.(map[string]any)
	result := make(map[string]string, len(object))
	for key, value := range object {
		if text, ok := value.(string); ok {
			result[key] = text
		}
	}
	return result
}

func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func literalMatchesField(value, fieldType string) bool {
	switch strings.ToLower(fieldType) {
	case "number", "integer", "decimal", "float":
		_, err := strconv.ParseFloat(value, 64)
		return err == nil
	case "boolean", "bool":
		_, err := strconv.ParseBool(value)
		return err == nil
	default:
		return true
	}
}

func variableRoot(variable string) string {
	if index := strings.IndexByte(variable, '.'); index >= 0 {
		return variable[:index]
	}
	return variable
}

func cloneBoolSet(source map[string]bool) map[string]bool {
	clone := make(map[string]bool, len(source))
	for key, value := range source {
		if value {
			clone[key] = true
		}
	}
	return clone
}

func unionBoolSets(left, right map[string]bool) map[string]bool {
	result := cloneBoolSet(left)
	for key := range right {
		result[key] = true
	}
	return result
}

func intersectBoolSets(left, right map[string]bool) map[string]bool {
	result := make(map[string]bool)
	for key := range left {
		if right[key] {
			result[key] = true
		}
	}
	return result
}

func equalBoolSets(left, right map[string]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for key := range left {
		if !right[key] {
			return false
		}
	}
	return true
}

func sortDiagnostics(diagnostics []Diagnostic) {
	sort.SliceStable(diagnostics, func(left, right int) bool {
		if diagnostics[left].Severity != diagnostics[right].Severity {
			return diagnostics[left].Severity < diagnostics[right].Severity
		}
		if diagnostics[left].Source != diagnostics[right].Source {
			return diagnostics[left].Source < diagnostics[right].Source
		}
		if diagnostics[left].NodeID != diagnostics[right].NodeID {
			return diagnostics[left].NodeID < diagnostics[right].NodeID
		}
		if diagnostics[left].Path != diagnostics[right].Path {
			return diagnostics[left].Path < diagnostics[right].Path
		}
		return diagnostics[left].Code < diagnostics[right].Code
	})
}
