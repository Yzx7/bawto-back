package authoring

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/Yzx7/sacs-chatbots/engine"
)

// OperationType is the closed set of candidate mutations exposed to the
// Copilot. There is intentionally no replace-document operation.
type OperationType string

const (
	OperationAddNode             OperationType = "add_node"
	OperationUpdateNodeConfig    OperationType = "update_node_config"
	OperationRemoveNode          OperationType = "remove_node"
	OperationConnectNodes        OperationType = "connect_nodes"
	OperationDisconnectNodes     OperationType = "disconnect_nodes"
	OperationUpdateTriggerConfig OperationType = "update_trigger_config"
)

// NodeReference addresses either an existing id or an alias created earlier
// in the same atomic batch. Exactly one field must be present.
type NodeReference struct {
	ID    string `json:"id,omitempty"`
	Alias string `json:"alias,omitempty"`
}

// EdgeReference follows the same batch-local rule as NodeReference.
type EdgeReference struct {
	ID    string `json:"id,omitempty"`
	Alias string `json:"alias,omitempty"`
}

type AddNodeOperation struct {
	Alias  string         `json:"alias"`
	Kind   string         `json:"kind"`
	Anchor *NodeReference `json:"anchor,omitempty"`
	Set    map[string]any `json:"set,omitempty"`
}

type UpdateNodeConfigOperation struct {
	Node       NodeReference  `json:"node"`
	Set        map[string]any `json:"set,omitempty"`
	UnsetPaths []string       `json:"unsetPaths,omitempty"`
}

type RemoveNodeOperation struct {
	Node NodeReference `json:"node"`
}

type ConnectNodesOperation struct {
	Alias        string        `json:"alias"`
	Source       NodeReference `json:"source"`
	Target       NodeReference `json:"target"`
	SourceHandle string        `json:"sourceHandle,omitempty"`
	Role         string        `json:"role,omitempty"`
	Tag          string        `json:"tag,omitempty"`
	Label        string        `json:"label,omitempty"`
}

type DisconnectNodesOperation struct {
	Edge EdgeReference `json:"edge"`
}

type UpdateTriggerConfigOperation struct {
	Set        map[string]any `json:"set,omitempty"`
	UnsetPaths []string       `json:"unsetPaths,omitempty"`
}

// FlowOperation is a tagged union. Exactly the payload matching Type must be
// present; this keeps operation schemas explicit and auditable.
type FlowOperation struct {
	Type                OperationType                 `json:"type"`
	AddNode             *AddNodeOperation             `json:"addNode,omitempty"`
	UpdateNodeConfig    *UpdateNodeConfigOperation    `json:"updateNodeConfig,omitempty"`
	RemoveNode          *RemoveNodeOperation          `json:"removeNode,omitempty"`
	ConnectNodes        *ConnectNodesOperation        `json:"connectNodes,omitempty"`
	DisconnectNodes     *DisconnectNodesOperation     `json:"disconnectNodes,omitempty"`
	UpdateTriggerConfig *UpdateTriggerConfigOperation `json:"updateTriggerConfig,omitempty"`
}

// EntityKind identifies the namespace passed to an ID generator.
type EntityKind string

const (
	EntityNode EntityKind = "node"
	EntityEdge EntityKind = "edge"
)

// IDGenerator is injectable for production UUIDs and deterministic tests. The
// attempt increases only when a generated id collides with the candidate.
type IDGenerator interface {
	GenerateID(kind EntityKind, alias string, attempt int) (string, error)
}

type IDGeneratorFunc func(kind EntityKind, alias string, attempt int) (string, error)

func (function IDGeneratorFunc) GenerateID(kind EntityKind, alias string, attempt int) (string, error) {
	return function(kind, alias, attempt)
}

type deterministicIDGenerator struct {
	seed string
}

func (generator deterministicIDGenerator) GenerateID(kind EntityKind, alias string, attempt int) (string, error) {
	sum := sha256.Sum256([]byte(generator.seed + "\x00" + string(kind) + "\x00" + alias + "\x00" + strconv.Itoa(attempt)))
	prefix := "n_"
	if kind == EntityEdge {
		prefix = "e_"
	}
	return prefix + hex.EncodeToString(sum[:8]), nil
}

// ApplyOptions binds a batch to the exact in-memory candidate it was planned
// against. ExpectedCandidateChecksum is mandatory.
type ApplyOptions struct {
	ExpectedCandidateChecksum string
	IDGenerator               IDGenerator
}

// ApplyResult is returned only when every operation and the final engine
// validation succeed.
type ApplyResult struct {
	Candidate         json.RawMessage   `json:"candidate"`
	PreviousChecksum  string            `json:"previousChecksum"`
	CandidateChecksum string            `json:"candidateChecksum"`
	AliasToNodeID     map[string]string `json:"aliasToNodeId"`
	AliasToEdgeID     map[string]string `json:"aliasToEdgeId"`
	Diff              FlowDiff          `json:"diff"`
}

var ErrCandidateChecksumMismatch = errors.New("candidate checksum mismatch")

type CandidateChecksumMismatchError struct {
	Expected string
	Actual   string
}

func (err *CandidateChecksumMismatchError) Error() string {
	return fmt.Sprintf("el candidato cambió: checksum esperado %s, actual %s", err.Expected, err.Actual)
}

func (err *CandidateChecksumMismatchError) Is(target error) bool {
	return target == ErrCandidateChecksumMismatch
}

// ApplyFlowOperations applies a batch to a detached generic document. On any
// error it returns no candidate, so callers cannot accidentally retain a
// partially-mutated workspace.
func ApplyFlowOperations(raw json.RawMessage, operations []FlowOperation, options ApplyOptions) (*ApplyResult, error) {
	_, actualChecksum, err := engine.CanonicalChecksum(raw)
	if err != nil {
		return nil, fmt.Errorf("checksum del candidato: %w", err)
	}
	if strings.TrimSpace(options.ExpectedCandidateChecksum) == "" {
		return nil, fmt.Errorf("expectedCandidateChecksum es obligatorio")
	}
	if options.ExpectedCandidateChecksum != actualChecksum {
		return nil, &CandidateChecksumMismatchError{Expected: options.ExpectedCandidateChecksum, Actual: actualChecksum}
	}

	document, err := ParseDocument(raw)
	if err != nil {
		return nil, err
	}
	if err := validateDocumentIdentity(document); err != nil {
		return nil, err
	}
	generator := options.IDGenerator
	if generator == nil {
		generator = deterministicIDGenerator{seed: actualChecksum}
	}
	workspace := operationWorkspace{
		document: document, generator: generator,
		nodeAliases: map[string]string{}, edgeAliases: map[string]string{},
	}
	for index, operation := range operations {
		if err := workspace.apply(operation); err != nil {
			return nil, fmt.Errorf("operación %d (%s): %w", index+1, operation.Type, err)
		}
	}

	candidate, err := MarshalDocument(workspace.document)
	if err != nil {
		return nil, err
	}
	if err := ValidateIntermediateCandidate(candidate); err != nil {
		return nil, fmt.Errorf("el candidato intermedio no es válido: %w", err)
	}
	_, candidateChecksum, err := engine.CanonicalChecksum(candidate)
	if err != nil {
		return nil, err
	}
	diff, err := SemanticDiff(raw, candidate)
	if err != nil {
		return nil, fmt.Errorf("diff del candidato: %w", err)
	}
	return &ApplyResult{
		Candidate: candidate, PreviousChecksum: actualChecksum, CandidateChecksum: candidateChecksum,
		AliasToNodeID: cloneStringMap(workspace.nodeAliases),
		AliasToEdgeID: cloneStringMap(workspace.edgeAliases), Diff: diff,
	}, nil
}

// ValidateIntermediateCandidate valida la estructura e integridad de cada nodo y arista
// sin requerir que el grafo esté completo y totalmente conectado (para mutaciones intermedias).
func ValidateIntermediateCandidate(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var flow engine.Flow
	if err := decoder.Decode(&flow); err != nil {
		return fmt.Errorf("vista tipada inválida: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("hay contenido después del flujo")
		}
		return fmt.Errorf("contenido extra inválido: %w", err)
	}
	return engine.ValidateStructural(&flow)
}

// ValidateCandidate builds only the typed view required by the runtime and
// delegates the structural decision to engine.Validate. The generic document
// remains the representation returned to the editor.
func ValidateCandidate(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var flow engine.Flow
	if err := decoder.Decode(&flow); err != nil {
		return fmt.Errorf("vista tipada inválida: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("hay contenido después del flujo")
		}
		return fmt.Errorf("contenido extra inválido: %w", err)
	}
	return engine.Validate(&flow)
}

type operationWorkspace struct {
	document    Document
	generator   IDGenerator
	nodeAliases map[string]string
	edgeAliases map[string]string
}

func (workspace *operationWorkspace) apply(operation FlowOperation) error {
	if err := operation.validateUnion(); err != nil {
		return err
	}
	switch operation.Type {
	case OperationAddNode:
		return workspace.addNode(*operation.AddNode)
	case OperationUpdateNodeConfig:
		return workspace.updateNode(*operation.UpdateNodeConfig)
	case OperationRemoveNode:
		return workspace.removeNode(*operation.RemoveNode)
	case OperationConnectNodes:
		return workspace.connectNodes(*operation.ConnectNodes)
	case OperationDisconnectNodes:
		return workspace.disconnectNodes(*operation.DisconnectNodes)
	case OperationUpdateTriggerConfig:
		return workspace.updateTrigger(*operation.UpdateTriggerConfig)
	default:
		return fmt.Errorf("tipo de operación %q desconocido", operation.Type)
	}
}

func (operation FlowOperation) validateUnion() error {
	present := 0
	for _, exists := range []bool{
		operation.AddNode != nil, operation.UpdateNodeConfig != nil,
		operation.RemoveNode != nil, operation.ConnectNodes != nil,
		operation.DisconnectNodes != nil, operation.UpdateTriggerConfig != nil,
	} {
		if exists {
			present++
		}
	}
	if present != 1 {
		return fmt.Errorf("la operación debe contener exactamente un payload")
	}
	matching := map[OperationType]bool{
		OperationAddNode:             operation.AddNode != nil,
		OperationUpdateNodeConfig:    operation.UpdateNodeConfig != nil,
		OperationRemoveNode:          operation.RemoveNode != nil,
		OperationConnectNodes:        operation.ConnectNodes != nil,
		OperationDisconnectNodes:     operation.DisconnectNodes != nil,
		OperationUpdateTriggerConfig: operation.UpdateTriggerConfig != nil,
	}
	if !matching[operation.Type] {
		return fmt.Errorf("el payload no corresponde a type=%q", operation.Type)
	}
	return nil
}

func (workspace *operationWorkspace) addNode(operation AddNodeOperation) error {
	alias, err := validateAlias(operation.Alias)
	if err != nil {
		return err
	}
	if _, exists := workspace.nodeAliases[alias]; exists {
		return fmt.Errorf("alias de nodo duplicado %q", alias)
	}
	spec, exists := GetNodeKind(operation.Kind)
	if !exists {
		return fmt.Errorf("tipo de nodo %q fuera del catálogo", operation.Kind)
	}
	nodes, err := objectArrayField(workspace.document, "nodes")
	if err != nil {
		return err
	}
	nodeIDs, err := indexByID(nodes, "nodo")
	if err != nil {
		return err
	}
	id, err := allocateID(workspace.generator, EntityNode, alias, nodeIDs)
	if err != nil {
		return err
	}
	defaults, err := cloneJSONValue(spec.Defaults)
	if err != nil {
		return err
	}
	node := defaults.(map[string]any)
	node["id"] = id
	node["kind"] = operation.Kind
	position, err := workspace.newNodePosition(operation.Anchor, nodes)
	if err != nil {
		return err
	}
	node["pos"] = position
	if err := applyNodePatch(node, operation.Kind, operation.Set, nil); err != nil {
		return err
	}
	nodes = append(nodes, node)
	storeObjectArray(workspace.document, "nodes", nodes)
	workspace.nodeAliases[alias] = id
	return nil
}

func (workspace *operationWorkspace) updateNode(operation UpdateNodeConfigOperation) error {
	nodes, err := objectArrayField(workspace.document, "nodes")
	if err != nil {
		return err
	}
	id, err := workspace.resolveNode(operation.Node, false)
	if err != nil {
		return err
	}
	node, exists := findByID(nodes, id)
	if !exists {
		return fmt.Errorf("el nodo %q no existe", id)
	}
	kind, ok := node["kind"].(string)
	if !ok {
		return fmt.Errorf("el nodo %q no declara kind", id)
	}
	return applyNodePatch(node, kind, operation.Set, operation.UnsetPaths)
}

func (workspace *operationWorkspace) removeNode(operation RemoveNodeOperation) error {
	nodes, err := objectArrayField(workspace.document, "nodes")
	if err != nil {
		return err
	}
	id, err := workspace.resolveNode(operation.Node, false)
	if err != nil {
		return err
	}
	index := -1
	for nodeIndex, node := range nodes {
		if node["id"] == id {
			index = nodeIndex
			break
		}
	}
	if index < 0 {
		return fmt.Errorf("el nodo %q no existe", id)
	}
	edges, err := objectArrayField(workspace.document, "edges")
	if err != nil {
		return err
	}
	connected := make([]string, 0)
	for _, edge := range edges {
		if edge["source"] == id || edge["target"] == id {
			edgeID, _ := edge["id"].(string)
			connected = append(connected, edgeID)
		}
	}
	if len(connected) > 0 {
		sort.Strings(connected)
		return fmt.Errorf("el nodo %q sigue conectado por las aristas %s; desconéctalas explícitamente antes", id, strings.Join(connected, ", "))
	}
	nodes = append(nodes[:index], nodes[index+1:]...)
	storeObjectArray(workspace.document, "nodes", nodes)
	return nil
}

func (workspace *operationWorkspace) connectNodes(operation ConnectNodesOperation) error {
	alias, err := validateAlias(operation.Alias)
	if err != nil {
		return err
	}
	if _, exists := workspace.edgeAliases[alias]; exists {
		return fmt.Errorf("alias de arista duplicado %q", alias)
	}
	source, err := workspace.resolveNode(operation.Source, true)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	target, err := workspace.resolveNode(operation.Target, false)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	nodes, err := objectArrayField(workspace.document, "nodes")
	if err != nil {
		return err
	}
	nodeIndex, err := indexByID(nodes, "nodo")
	if err != nil {
		return err
	}
	if source != "trigger" {
		if _, exists := nodeIndex[source]; !exists {
			return fmt.Errorf("el nodo source %q no existe", source)
		}
	}
	if _, exists := nodeIndex[target]; !exists {
		return fmt.Errorf("el nodo target %q no existe", target)
	}
	handle, err := workspace.connectionHandle(source, operation.SourceHandle, nodeIndex)
	if err != nil {
		return err
	}
	if operation.Role != "" && operation.Role != "loopback" {
		return fmt.Errorf("role %q inválido", operation.Role)
	}
	edges, err := objectArrayField(workspace.document, "edges")
	if err != nil {
		return err
	}
	edgeIDs, err := indexByID(edges, "arista")
	if err != nil {
		return err
	}
	id, err := allocateID(workspace.generator, EntityEdge, alias, edgeIDs)
	if err != nil {
		return err
	}
	edge := map[string]any{"id": id, "source": source, "target": target}
	if handle != "" {
		edge["sourceHandle"] = handle
	}
	if operation.Role != "" {
		edge["role"] = operation.Role
	}
	if operation.Tag != "" {
		edge["tag"] = operation.Tag
	}
	if operation.Label != "" {
		edge["label"] = operation.Label
	}
	edges = append(edges, edge)
	storeObjectArray(workspace.document, "edges", edges)
	workspace.edgeAliases[alias] = id
	return nil
}

func (workspace *operationWorkspace) disconnectNodes(operation DisconnectNodesOperation) error {
	edges, err := objectArrayField(workspace.document, "edges")
	if err != nil {
		return err
	}
	id, err := workspace.resolveEdge(operation.Edge)
	if err != nil {
		return err
	}
	index := -1
	for edgeIndex, edge := range edges {
		if edge["id"] == id {
			index = edgeIndex
			break
		}
	}
	if index < 0 {
		return fmt.Errorf("la arista %q no existe", id)
	}
	edges = append(edges[:index], edges[index+1:]...)
	storeObjectArray(workspace.document, "edges", edges)
	return nil
}

func (workspace *operationWorkspace) updateTrigger(operation UpdateTriggerConfigOperation) error {
	trigger, err := objectField(workspace.document, "trigger")
	if err != nil {
		return err
	}
	triggerType, ok := trigger["type"].(string)
	if !ok || triggerType == "" {
		return fmt.Errorf("trigger.type es obligatorio")
	}
	return applyTriggerPatch(trigger, triggerType, operation.Set, operation.UnsetPaths)
}

func (workspace *operationWorkspace) resolveNode(reference NodeReference, allowTrigger bool) (string, error) {
	if (reference.ID == "") == (reference.Alias == "") {
		return "", fmt.Errorf("la referencia requiere exactamente id o alias")
	}
	if reference.Alias != "" {
		id, exists := workspace.nodeAliases[reference.Alias]
		if !exists {
			return "", fmt.Errorf("alias de nodo %q no existe en este lote", reference.Alias)
		}
		return id, nil
	}
	if reference.ID == "trigger" {
		if allowTrigger {
			return reference.ID, nil
		}
		return "", fmt.Errorf("trigger no puede usarse aquí")
	}
	return reference.ID, nil
}

func (workspace *operationWorkspace) resolveEdge(reference EdgeReference) (string, error) {
	if (reference.ID == "") == (reference.Alias == "") {
		return "", fmt.Errorf("la referencia requiere exactamente id o alias")
	}
	if reference.Alias != "" {
		id, exists := workspace.edgeAliases[reference.Alias]
		if !exists {
			return "", fmt.Errorf("alias de arista %q no existe en este lote", reference.Alias)
		}
		return id, nil
	}
	return reference.ID, nil
}

func (workspace *operationWorkspace) connectionHandle(source, requested string, nodes map[string]map[string]any) (string, error) {
	var ports []OutputPort
	var err error
	if source == "trigger" {
		ports, err = resolveTriggerPorts(workspace.document)
	} else {
		ports, err = ResolveOutputPorts(nodes[source])
	}
	if err != nil {
		return "", fmt.Errorf("puertos de %s: %w", source, err)
	}
	for _, port := range ports {
		if requested == port.ID || requested == port.Handle {
			return port.Handle, nil
		}
	}
	available := make([]string, 0, len(ports))
	for _, port := range ports {
		available = append(available, port.ID)
	}
	return "", fmt.Errorf("el puerto %q no existe en %s; disponibles: %s", requested, source, strings.Join(available, ", "))
}

func (workspace *operationWorkspace) newNodePosition(anchor *NodeReference, nodes []map[string]any) (map[string]any, error) {
	x, y := float64((len(nodes)+1)*320), 0.0
	if anchor != nil {
		anchorID, err := workspace.resolveNode(*anchor, false)
		if err != nil {
			return nil, fmt.Errorf("anchor: %w", err)
		}
		anchorNode, exists := findByID(nodes, anchorID)
		if !exists {
			return nil, fmt.Errorf("el nodo anchor %q no existe", anchorID)
		}
		if position, ok := anchorNode["pos"].(map[string]any); ok {
			if anchorX, ok := jsonCoordinate(position["x"]); ok {
				x = anchorX + 320
			}
			if anchorY, ok := jsonCoordinate(position["y"]); ok {
				y = anchorY
			}
		}
	}
	for collisionAttempt := 0; collisionAttempt < 200; collisionAttempt++ {
		if !positionCollides(nodes, x, y) {
			return map[string]any{"x": x, "y": y}, nil
		}
		y += 180
	}
	return nil, fmt.Errorf("no se encontró una posición libre para el nodo nuevo")
}

func positionCollides(nodes []map[string]any, x, y float64) bool {
	for _, node := range nodes {
		position, ok := node["pos"].(map[string]any)
		if !ok {
			continue
		}
		nodeX, xOK := jsonCoordinate(position["x"])
		nodeY, yOK := jsonCoordinate(position["y"])
		if xOK && yOK && math.Abs(nodeX-x) < 240 && math.Abs(nodeY-y) < 140 {
			return true
		}
	}
	return false
}

func jsonCoordinate(value any) (float64, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	case float64:
		return number, true
	case int:
		return float64(number), true
	default:
		return 0, false
	}
}

func applyNodePatch(node map[string]any, kind string, set map[string]any, unsetPaths []string) error {
	return applyPatch(node, set, unsetPaths, func(path string) (FieldSpec, string, bool) {
		return editableField(kind, path)
	})
}

func applyTriggerPatch(trigger map[string]any, triggerType string, set map[string]any, unsetPaths []string) error {
	fields, exists := triggerFields[triggerType]
	if !exists {
		return fmt.Errorf("tipo de trigger %q fuera del catálogo", triggerType)
	}
	resolve := func(path string) (FieldSpec, string, bool) {
		if path == "type" {
			return FieldSpec{}, "", false
		}
		for _, field := range fields {
			if path == field.Path {
				return field, "", true
			}
		}
		return FieldSpec{}, "", false
	}
	if _, wantsType := set["type"]; wantsType || containsString(unsetPaths, "type") {
		return fmt.Errorf("trigger.type es inmutable en el editor")
	}
	return applyPatch(trigger, set, unsetPaths, resolve)
}

var triggerFields = map[string][]FieldSpec{
	"message": {
		{Path: "match", Type: FieldString, AllowedValues: []string{"any", "keyword"}},
		{Path: "keywords", Type: FieldStringList},
		{Path: "accepts", Type: FieldStringList, AllowedValues: messageInputTypes},
		{Path: "routeBy", Type: FieldString, AllowedValues: []string{"single", "content_type"}},
	},
	"schedule": {
		{Path: "cron", Type: FieldString},
		{Path: "timezone", Type: FieldString},
		{Path: "viewId", Type: FieldString},
		{Path: "replyIntent", Type: FieldString},
	},
	"event": {
		{Path: "eventKey", Type: FieldString},
	},
}

var messageInputTypes = []string{
	"text", "image", "audio", "document", "video", "sticker", "location",
	"contacts", "interactive", "order", "reaction", "unsupported",
}

func applyPatch(target map[string]any, set map[string]any, unsetPaths []string, resolve func(string) (FieldSpec, string, bool)) error {
	if err := validatePatchPaths(set, unsetPaths); err != nil {
		return err
	}
	setPaths := make([]string, 0, len(set))
	for path := range set {
		setPaths = append(setPaths, path)
	}
	sort.Strings(setPaths)
	for _, path := range setPaths {
		field, child, allowed := resolve(path)
		if !allowed {
			return fmt.Errorf("set path %q no está permitido por el catálogo", path)
		}
		value, err := cloneJSONValue(set[path])
		if err != nil {
			return fmt.Errorf("set %s: %w", path, err)
		}
		if err := validateFieldValue(field, child, value); err != nil {
			return err
		}
		if child == "" {
			target[field.Path] = value
			continue
		}
		parent, exists := target[field.Path]
		if !exists {
			parent = map[string]any{}
			target[field.Path] = parent
		}
		object, ok := parent.(map[string]any)
		if !ok {
			return fmt.Errorf("%s debe ser un objeto para editar %s", field.Path, path)
		}
		object[child] = value
	}
	paths := append([]string(nil), unsetPaths...)
	sort.Strings(paths)
	for _, path := range paths {
		field, child, allowed := resolve(path)
		if !allowed {
			return fmt.Errorf("unset path %q no está permitido por el catálogo", path)
		}
		if child == "" {
			delete(target, field.Path)
			continue
		}
		if object, ok := target[field.Path].(map[string]any); ok {
			delete(object, child)
		}
	}
	return nil
}

func validatePatchPaths(set map[string]any, unsetPaths []string) error {
	seen := make(map[string]string, len(set)+len(unsetPaths))
	paths := make([]string, 0, len(set)+len(unsetPaths))
	for path := range set {
		if path == "" || strings.TrimSpace(path) != path {
			return fmt.Errorf("set contiene una ruta vacía o con espacios")
		}
		seen[path] = "set"
		paths = append(paths, path)
	}
	for _, path := range unsetPaths {
		if path == "" || strings.TrimSpace(path) != path {
			return fmt.Errorf("unsetPaths contiene una ruta vacía o con espacios")
		}
		if previous, exists := seen[path]; exists {
			return fmt.Errorf("la ruta %q aparece en %s y unsetPaths", path, previous)
		}
		seen[path] = "unset"
		paths = append(paths, path)
	}
	for left := 0; left < len(paths); left++ {
		for right := left + 1; right < len(paths); right++ {
			if strings.HasPrefix(paths[left], paths[right]+".") || strings.HasPrefix(paths[right], paths[left]+".") {
				return fmt.Errorf("las rutas %q y %q se solapan", paths[left], paths[right])
			}
		}
	}
	return nil
}

func resolveTriggerPorts(document Document) ([]OutputPort, error) {
	trigger, err := objectField(document, "trigger")
	if err != nil {
		return nil, err
	}
	triggerType, _ := trigger["type"].(string)
	if triggerType == "message" && trigger["routeBy"] == "content_type" {
		accepts, err := stringList(trigger["accepts"])
		if err != nil {
			return nil, err
		}
		return branchOutputs(accepts...), nil
	}
	return linearOutput(), nil
}

func validateDocumentIdentity(document Document) error {
	if _, err := objectField(document, "trigger"); err != nil {
		return err
	}
	nodes, err := objectArrayField(document, "nodes")
	if err != nil {
		return err
	}
	edges, err := objectArrayField(document, "edges")
	if err != nil {
		return err
	}
	if _, err := indexByID(nodes, "nodo"); err != nil {
		return err
	}
	if _, err := indexByID(edges, "arista"); err != nil {
		return err
	}
	return nil
}

func indexByID(items []map[string]any, entity string) (map[string]map[string]any, error) {
	result := make(map[string]map[string]any, len(items))
	for index, item := range items {
		id, ok := item["id"].(string)
		if !ok || strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("%s %d requiere id", entity, index+1)
		}
		if _, exists := result[id]; exists {
			return nil, fmt.Errorf("id de %s duplicado %q", entity, id)
		}
		result[id] = item
	}
	return result, nil
}

func findByID(items []map[string]any, id string) (map[string]any, bool) {
	for _, item := range items {
		if item["id"] == id {
			return item, true
		}
	}
	return nil, false
}

func allocateID(generator IDGenerator, kind EntityKind, alias string, existing map[string]map[string]any) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		id, err := generator.GenerateID(kind, alias, attempt)
		if err != nil {
			return "", fmt.Errorf("generar id para %q: %w", alias, err)
		}
		if strings.TrimSpace(id) == "" || id == "trigger" {
			return "", fmt.Errorf("el generador devolvió un id inválido para %q", alias)
		}
		if _, collision := existing[id]; !collision {
			return id, nil
		}
	}
	return "", fmt.Errorf("no se pudo asignar un id único para %q", alias)
}

func validateAlias(alias string) (string, error) {
	if strings.TrimSpace(alias) == "" || strings.TrimSpace(alias) != alias {
		return "", fmt.Errorf("alias vacío o con espacios exteriores")
	}
	if alias == "trigger" {
		return "", fmt.Errorf("alias %q reservado", alias)
	}
	return alias, nil
}

func cloneStringMap(source map[string]string) map[string]string {
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
