package copilot

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Yzx7/sacs-chatbots/authoring"
)

const (
	ToolGetFlowOutline         = "get_flow_outline"
	ToolGetNodes               = "get_nodes"
	ToolGetAuthoringCatalog    = "get_authoring_catalog"
	ToolGetRuntimeToolCatalog  = "get_runtime_tool_catalog"
	ToolListDataObjects        = "list_data_objects"
	ToolGetDataObjectSchema    = "get_data_object_schema"
	ToolListContactFields      = "list_contact_fields"
	ToolListConnectionsSafe    = "list_connections_safe"
	ToolListWhatsAppTemplates  = "list_whatsapp_templates"
	ToolListTemplates          = ToolListWhatsAppTemplates
	ToolListVariables          = "list_variables"
	ToolInspectVariablesAtNode = "inspect_variables_at_node"
	ToolListPlaybooks          = "list_playbooks"
	ToolGetPlaybook            = "get_playbook"
	ToolValidateCandidate      = "validate_candidate"
	ToolApplyFlowOperations    = "apply_flow_operations"
	ToolSubmitProposal         = "submit_proposal"
)

type FunctionDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type FlowOutline struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Trigger map[string]any `json:"trigger"`
	Nodes   []OutlineNode  `json:"nodes"`
	Edges   []OutlineEdge  `json:"edges"`
}

type OutlineNode struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Outputs []string `json:"outputs,omitempty"`
}

type OutlineEdge struct {
	ID           string `json:"id"`
	Source       string `json:"source"`
	Target       string `json:"target"`
	SourceHandle string `json:"sourceHandle,omitempty"`
	Role         string `json:"role,omitempty"`
}

// AuthoringFunctionDefinitions is a closed, executor-free function catalog.
// No entry can publish, send a message, execute a runtime tool, SQL or HTTP.
func AuthoringFunctionDefinitions() []FunctionDefinition {
	emptyObject := func() map[string]any {
		return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
	}
	pageProperties := map[string]any{
		"cursor": map[string]any{"type": "string"},
		"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
	}
	definitions := []FunctionDefinition{
		{Name: ToolGetFlowOutline, Description: "Resume nodos, puertos y conexiones del candidato actual.", InputSchema: emptyObject()},
		{Name: ToolGetNodes, Description: "Devuelve la configuración completa de nodos concretos.", InputSchema: objectSchema(map[string]any{
			"ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": 20},
		}, []string{"ids"})},
		{Name: ToolGetAuthoringCatalog, Description: "Lista node kinds, campos editables, defaults y puertos reales.", InputSchema: emptyObject()},
		{Name: ToolGetRuntimeToolCatalog, Description: "Lista specs de engine/tools sin ejecutores.", InputSchema: emptyObject()},
		{Name: ToolListDataObjects, Description: "Lista objetos de Data sin filas.", InputSchema: objectSchema(pageProperties, nil)},
		{Name: ToolGetDataObjectSchema, Description: "Devuelve el schema completo de un objeto por clave.", InputSchema: objectSchema(map[string]any{
			"key": map[string]any{"type": "string"},
		}, []string{"key"})},
		{Name: ToolListContactFields, Description: "Lista campos personalizados de contacto sin valores.", InputSchema: objectSchema(pageProperties, nil)},
		{Name: ToolListConnectionsSafe, Description: "Lista solo clave, etiqueta, driver, capacidades, estado y lastOKAt.", InputSchema: objectSchema(pageProperties, nil)},
		{Name: ToolListWhatsAppTemplates, Description: "Lista plantillas de WhatsApp conocidas y su estado.", InputSchema: objectSchema(pageProperties, nil)},
		{Name: ToolListVariables, Description: "Lista variables configuradas sin valores privados.", InputSchema: objectSchema(pageProperties, nil)},
		{Name: ToolInspectVariablesAtNode, Description: "Calcula variables definidas o quizá definidas antes de un nodo.", InputSchema: objectSchema(map[string]any{
			"nodeId": map[string]any{"type": "string"},
		}, []string{"nodeId"})},
		{Name: ToolListPlaybooks, Description: "Lista playbooks embebidos con versión y hash transitivo.", InputSchema: emptyObject()},
		{Name: ToolGetPlaybook, Description: "Obtiene un bundle de conocimiento exacto.", InputSchema: objectSchema(map[string]any{
			"id": map[string]any{"type": "string"}, "version": map[string]any{"type": "string"},
		}, []string{"id"})},
		{Name: ToolValidateCandidate, Description: "Ejecuta engine, bindings y lint sobre el workspace actual.", InputSchema: emptyObject()},
		{Name: ToolApplyFlowOperations, Description: "Aplica atómicamente operaciones tipadas a la copia candidata; nunca guarda ni publica.", InputSchema: applyOperationsSchema()},
		{Name: ToolSubmitProposal, Description: "Terminal obligatorio: pregunta, explicación o propuesta validada.", InputSchema: submitProposalSchema()},
	}
	return definitions
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func applyOperationsSchema() map[string]any {
	operationTypes := []string{
		string(authoring.OperationAddNode), string(authoring.OperationUpdateNodeConfig),
		string(authoring.OperationRemoveNode), string(authoring.OperationConnectNodes),
		string(authoring.OperationDisconnectNodes), string(authoring.OperationUpdateTriggerConfig),
	}

	nodeRefSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":    map[string]any{"type": "string", "description": "ID existente de nodo o 'trigger'"},
			"alias": map[string]any{"type": "string", "description": "Alias local de nodo creado en este lote"},
		},
		"additionalProperties": false,
	}

	edgeRefSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":    map[string]any{"type": "string", "description": "ID existente de arista"},
			"alias": map[string]any{"type": "string", "description": "Alias local de arista creado en este lote"},
		},
		"additionalProperties": false,
	}

	addNodeSchema := map[string]any{
		"type":        "object",
		"description": "Agrega un nodo nuevo. Sus propiedades van dentro de 'set'.",
		"properties": map[string]any{
			"alias":  map[string]any{"type": "string", "description": "Alias local único del nodo (ej: 'agente_1', 'send_bienvenida')"},
			"kind":   map[string]any{"type": "string", "enum": []string{"send", "agent", "wait", "tool", "router", "condition", "action"}, "description": "Tipo de nodo"},
			"anchor": nodeRefSchema,
			"set": map[string]any{
				"type":        "object",
				"description": "Propiedades del nodo. Para 'send': {body: 'texto'}. Para 'agent': {instruction: '...', outputs: ['ok','error']}. Para 'action': {action: 'end'|'handoff'|'set'}. Para 'router': {cases: [{id:'c1', label:'...', expression:'...'}]}.",
			},
		},
		"required":             []string{"alias", "kind"},
		"additionalProperties": false,
	}

	connectNodesSchema := map[string]any{
		"type":        "object",
		"description": "Conecta una arista entre origen y destino.",
		"properties": map[string]any{
			"alias":        map[string]any{"type": "string", "description": "Alias local de la arista (ej: 'e1')"},
			"source":       nodeRefSchema,
			"target":       nodeRefSchema,
			"sourceHandle": map[string]any{"type": "string", "description": "Puerto de salida de origen: 'out' (send/wait/action), 'ok'/'error' (agent/tool), 'default'/'caso_id' (router), 'true'/'false' (condition)"},
			"role":         map[string]any{"type": "string", "enum": []string{"", "loopback"}},
			"label":        map[string]any{"type": "string"},
		},
		"required":             []string{"alias", "source", "target"},
		"additionalProperties": false,
	}

	updateNodeConfigSchema := map[string]any{
		"type":        "object",
		"description": "Actualiza propiedades de un nodo existente.",
		"properties": map[string]any{
			"node":       nodeRefSchema,
			"set":        map[string]any{"type": "object", "description": "Propiedades a modificar"},
			"unsetPaths": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required":             []string{"node"},
		"additionalProperties": false,
	}

	removeNodeSchema := map[string]any{
		"type":        "object",
		"description": "Elimina un nodo desconectado.",
		"properties": map[string]any{
			"node": nodeRefSchema,
		},
		"required":             []string{"node"},
		"additionalProperties": false,
	}

	disconnectNodesSchema := map[string]any{
		"type":        "object",
		"description": "Elimina una arista existente.",
		"properties": map[string]any{
			"edge": edgeRefSchema,
		},
		"required":             []string{"edge"},
		"additionalProperties": false,
	}

	updateTriggerConfigSchema := map[string]any{
		"type":        "object",
		"description": "Actualiza la configuración del trigger.",
		"properties": map[string]any{
			"set":        map[string]any{"type": "object"},
			"unsetPaths": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"additionalProperties": false,
	}

	return objectSchema(map[string]any{
		"expectedCandidateChecksum": map[string]any{"type": "string", "description": "Checksum SHA256 actual del candidato"},
		"operations": map[string]any{
			"type": "array", "minItems": 1, "maxItems": 60,
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type":                map[string]any{"type": "string", "enum": operationTypes},
					"addNode":             addNodeSchema,
					"updateNodeConfig":    updateNodeConfigSchema,
					"removeNode":          removeNodeSchema,
					"connectNodes":        connectNodesSchema,
					"disconnectNodes":     disconnectNodesSchema,
					"updateTriggerConfig": updateTriggerConfigSchema,
				},
				"required":             []string{"type"},
				"additionalProperties": false,
			},
		},
	}, []string{"expectedCandidateChecksum", "operations"})
}

func submitProposalSchema() map[string]any {
	stringList := map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 20}
	return objectSchema(map[string]any{
		"mode":                      map[string]any{"type": "string", "enum": []string{string(TerminalQuestion), string(TerminalExplanation), string(TerminalProposal)}},
		"response":                  map[string]any{"type": "string"},
		"assumptions":               stringList,
		"pendingDecisions":          stringList,
		"intentSummary":             map[string]any{"type": "string"},
		"warnings":                  stringList,
		"expectedCandidateChecksum": map[string]any{"type": "string"},
		"playbooks": map[string]any{
			"type": "array", "items": objectSchema(map[string]any{
				"id": map[string]any{"type": "string"}, "version": map[string]any{"type": "string"},
			}, []string{"id", "version"}),
		},
	}, []string{"mode", "response"})
}

func flowOutline(raw []byte) (FlowOutline, error) {
	document, err := authoring.ParseDocument(raw)
	if err != nil {
		return FlowOutline{}, err
	}
	id, _ := document["id"].(string)
	name, _ := document["name"].(string)
	trigger, ok := document["trigger"].(map[string]any)
	if !ok {
		return FlowOutline{}, fmt.Errorf("trigger inválido")
	}
	nodeValues, ok := document["nodes"].([]any)
	if !ok {
		return FlowOutline{}, fmt.Errorf("nodes inválido")
	}
	edgeValues, ok := document["edges"].([]any)
	if !ok {
		return FlowOutline{}, fmt.Errorf("edges inválido")
	}
	outline := FlowOutline{ID: id, Name: name, Trigger: trigger}
	for _, value := range nodeValues {
		node, ok := value.(map[string]any)
		if !ok {
			return FlowOutline{}, fmt.Errorf("nodo inválido")
		}
		nodeID, _ := node["id"].(string)
		kind, _ := node["kind"].(string)
		ports, _ := authoring.ResolveOutputPorts(node)
		entry := OutlineNode{ID: nodeID, Kind: kind}
		for _, port := range ports {
			entry.Outputs = append(entry.Outputs, port.ID)
		}
		outline.Nodes = append(outline.Nodes, entry)
	}
	for _, value := range edgeValues {
		edge, ok := value.(map[string]any)
		if !ok {
			return FlowOutline{}, fmt.Errorf("arista inválida")
		}
		entry := OutlineEdge{}
		entry.ID, _ = edge["id"].(string)
		entry.Source, _ = edge["source"].(string)
		entry.Target, _ = edge["target"].(string)
		entry.SourceHandle, _ = edge["sourceHandle"].(string)
		entry.Role, _ = edge["role"].(string)
		outline.Edges = append(outline.Edges, entry)
	}
	return outline, nil
}

type pageInput struct {
	Cursor string `json:"cursor"`
	Limit  int    `json:"limit"`
}

type pageResult[T any] struct {
	Items     []T    `json:"items"`
	Total     int    `json:"total"`
	HasMore   bool   `json:"hasMore"`
	Cursor    string `json:"cursor,omitempty"`
	Truncated bool   `json:"truncated"`
}

func paginate[T any](items []T, input pageInput) pageResult[T] {
	start, _ := strconv.Atoi(input.Cursor)
	if start < 0 || start > len(items) {
		start = 0
	}
	limit := input.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	result := pageResult[T]{Items: append([]T(nil), items[start:end]...), Total: len(items), HasMore: end < len(items), Truncated: end < len(items)}
	if result.HasMore {
		result.Cursor = strconv.Itoa(end)
	}
	return result
}

func decodeArguments(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("argumentos inválidos: %w", err)
	}
	return nil
}

func sortedDataObjects(items []authoring.DataObjectResource) []authoring.DataObjectResource {
	result := append([]authoring.DataObjectResource(nil), items...)
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result
}
