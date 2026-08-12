package authoring

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Yzx7/sacs-chatbots/engine"
)

var baseCandidate = json.RawMessage(`{
  "id": "flow-base",
  "name": "Flujo base",
  "editorExtension": {"large": 9007199254740993, "nested": [1, {"keep": true}]},
  "trigger": {"type": "message", "match": "any", "editorHint": "keep-trigger"},
  "nodes": [
    {
      "id": "start", "kind": "send", "pos": {"x": 10, "y": 20},
      "body": "Hola", "templateName": "legacy", "templateLanguage": "es",
      "editorMeta": {"collapsed": false, "keep": "node"}
    },
    {
      "id": "end", "kind": "action", "pos": {"x": 400, "y": 20},
      "action": "end", "editorMeta": {"keep": "end"}
    }
  ],
  "edges": [
    {"id": "e-trigger-start", "source": "trigger", "target": "start", "editorMeta": "root-edge"},
    {"id": "e-start-end", "source": "start", "target": "end", "label": "visible", "editorMeta": {"keep": true}}
  ]
}`)

func checksumForTest(t *testing.T, raw []byte) string {
	t.Helper()
	_, checksum, err := engine.CanonicalChecksum(raw)
	if err != nil {
		t.Fatal(err)
	}
	return checksum
}

func TestApplyFlowOperationsPreservesUnknownFieldsAndPositions(t *testing.T) {
	originalBytes := append([]byte(nil), baseCandidate...)
	result, err := ApplyFlowOperations(baseCandidate, []FlowOperation{{
		Type: OperationUpdateNodeConfig,
		UpdateNodeConfig: &UpdateNodeConfigOperation{
			Node:       NodeReference{ID: "start"},
			Set:        map[string]any{"body": "Hola actualizado"},
			UnsetPaths: []string{"templateName", "templateLanguage"},
		},
	}}, ApplyOptions{ExpectedCandidateChecksum: checksumForTest(t, baseCandidate)})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(baseCandidate, originalBytes) {
		t.Fatal("el lote mutó los bytes de entrada")
	}
	before, _ := ParseDocument(baseCandidate)
	after, _ := ParseDocument(result.Candidate)
	if !reflect.DeepEqual(before["editorExtension"], after["editorExtension"]) {
		t.Fatalf("extensión raíz cambió: %#v -> %#v", before["editorExtension"], after["editorExtension"])
	}
	beforeNodes, _ := objectArrayField(before, "nodes")
	afterNodes, _ := objectArrayField(after, "nodes")
	beforeStart, _ := findByID(beforeNodes, "start")
	afterStart, _ := findByID(afterNodes, "start")
	if !reflect.DeepEqual(beforeStart["pos"], afterStart["pos"]) || !reflect.DeepEqual(beforeStart["editorMeta"], afterStart["editorMeta"]) {
		t.Fatalf("posición o metadata ajena cambiaron: %#v", afterStart)
	}
	if afterStart["body"] != "Hola actualizado" {
		t.Fatalf("body=%v", afterStart["body"])
	}
	if _, exists := afterStart["templateName"]; exists {
		t.Fatal("templateName no fue eliminado")
	}
	if err := ValidateCandidate(result.Candidate); err != nil {
		t.Fatal(err)
	}
	if len(result.Diff.Nodes) != 1 || result.Diff.Nodes[0].ID != "start" || result.Diff.Nodes[0].Type != EntityModified {
		t.Fatalf("diff inesperado: %+v", result.Diff)
	}
}

func TestApplyFlowOperationsUsesBatchAliasesAndDeterministicIDs(t *testing.T) {
	operations := []FlowOperation{
		{
			Type:            OperationDisconnectNodes,
			DisconnectNodes: &DisconnectNodesOperation{Edge: EdgeReference{ID: "e-start-end"}},
		},
		{
			Type: OperationAddNode,
			AddNode: &AddNodeOperation{
				Alias: "pause", Kind: "wait", Anchor: &NodeReference{ID: "start"},
				Set: map[string]any{"saveAs": "incoming"},
			},
		},
		{
			Type: OperationConnectNodes,
			ConnectNodes: &ConnectNodesOperation{
				Alias: "start-pause", Source: NodeReference{ID: "start"},
				Target: NodeReference{Alias: "pause"}, SourceHandle: "out",
			},
		},
		{
			Type: OperationConnectNodes,
			ConnectNodes: &ConnectNodesOperation{
				Alias: "pause-end", Source: NodeReference{Alias: "pause"},
				Target: NodeReference{ID: "end"}, SourceHandle: "out",
			},
		},
	}
	options := ApplyOptions{ExpectedCandidateChecksum: checksumForTest(t, baseCandidate)}
	first, err := ApplyFlowOperations(baseCandidate, operations, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ApplyFlowOperations(baseCandidate, operations, options)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Candidate, second.Candidate) || !reflect.DeepEqual(first.AliasToNodeID, second.AliasToNodeID) || !reflect.DeepEqual(first.AliasToEdgeID, second.AliasToEdgeID) {
		t.Fatalf("resultado no determinista:\nfirst=%s\nsecond=%s", first.Candidate, second.Candidate)
	}
	pauseID := first.AliasToNodeID["pause"]
	if pauseID == "" || first.AliasToEdgeID["start-pause"] == "" || first.AliasToEdgeID["pause-end"] == "" {
		t.Fatalf("aliases incompletos: nodes=%v edges=%v", first.AliasToNodeID, first.AliasToEdgeID)
	}
	document, _ := ParseDocument(first.Candidate)
	nodes, _ := objectArrayField(document, "nodes")
	pause, exists := findByID(nodes, pauseID)
	if !exists || pause["saveAs"] != "incoming" {
		t.Fatalf("nodo por alias no creado: %#v", pause)
	}
	if reflect.DeepEqual(pause["pos"], map[string]any{"x": json.Number("10"), "y": json.Number("20")}) {
		t.Fatal("el nodo nuevo se superpuso con su ancla")
	}
}

func TestApplyFlowOperationsSupportsInjectedIDs(t *testing.T) {
	generator := IDGeneratorFunc(func(kind EntityKind, alias string, attempt int) (string, error) {
		return string(kind) + "-" + alias, nil
	})
	operations := []FlowOperation{
		{Type: OperationDisconnectNodes, DisconnectNodes: &DisconnectNodesOperation{Edge: EdgeReference{ID: "e-start-end"}}},
		{Type: OperationAddNode, AddNode: &AddNodeOperation{Alias: "pause", Kind: "wait"}},
		{Type: OperationConnectNodes, ConnectNodes: &ConnectNodesOperation{Alias: "one", Source: NodeReference{ID: "start"}, Target: NodeReference{Alias: "pause"}, SourceHandle: "out"}},
		{Type: OperationConnectNodes, ConnectNodes: &ConnectNodesOperation{Alias: "two", Source: NodeReference{Alias: "pause"}, Target: NodeReference{ID: "end"}, SourceHandle: "out"}},
	}
	result, err := ApplyFlowOperations(baseCandidate, operations, ApplyOptions{
		ExpectedCandidateChecksum: checksumForTest(t, baseCandidate), IDGenerator: generator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AliasToNodeID["pause"] != "node-pause" || result.AliasToEdgeID["one"] != "edge-one" {
		t.Fatalf("ids inyectados no usados: nodes=%v edges=%v", result.AliasToNodeID, result.AliasToEdgeID)
	}
}

func TestApplyFlowOperationsIsAtomicOnInvalidCandidate(t *testing.T) {
	original := append([]byte(nil), baseCandidate...)
	result, err := ApplyFlowOperations(baseCandidate, []FlowOperation{{
		Type: OperationUpdateNodeConfig,
		UpdateNodeConfig: &UpdateNodeConfigOperation{
			Node:       NodeReference{ID: "start"},
			UnsetPaths: []string{"body", "templateName", "templateLanguage"},
		},
	}}, ApplyOptions{ExpectedCandidateChecksum: checksumForTest(t, baseCandidate)})
	if err == nil || result != nil || !strings.Contains(err.Error(), "requiere texto o plantilla") {
		t.Fatalf("result=%v err=%v", result, err)
	}
	if !bytes.Equal(baseCandidate, original) {
		t.Fatal("un lote inválido mutó la entrada")
	}
}

func TestApplyFlowOperationsRejectsTriggerTypeAndStaleChecksum(t *testing.T) {
	result, err := ApplyFlowOperations(baseCandidate, []FlowOperation{{
		Type:                OperationUpdateTriggerConfig,
		UpdateTriggerConfig: &UpdateTriggerConfigOperation{Set: map[string]any{"type": "schedule"}},
	}}, ApplyOptions{ExpectedCandidateChecksum: checksumForTest(t, baseCandidate)})
	if err == nil || result != nil || !strings.Contains(err.Error(), "trigger.type es inmutable") {
		t.Fatalf("result=%v err=%v", result, err)
	}
	_, err = ApplyFlowOperations(baseCandidate, nil, ApplyOptions{ExpectedCandidateChecksum: "stale"})
	if !errors.Is(err, ErrCandidateChecksumMismatch) {
		t.Fatalf("err=%v", err)
	}
}

func TestApplyFlowOperationsRejectsConnectedRemovalAndAutomaticCycle(t *testing.T) {
	result, err := ApplyFlowOperations(baseCandidate, []FlowOperation{{
		Type:       OperationRemoveNode,
		RemoveNode: &RemoveNodeOperation{Node: NodeReference{ID: "start"}},
	}}, ApplyOptions{ExpectedCandidateChecksum: checksumForTest(t, baseCandidate)})
	if err == nil || result != nil || !strings.Contains(err.Error(), "sigue conectado") {
		t.Fatalf("result=%v err=%v", result, err)
	}
	result, err = ApplyFlowOperations(baseCandidate, []FlowOperation{
		{
			Type: OperationUpdateNodeConfig,
			UpdateNodeConfig: &UpdateNodeConfigOperation{
				Node: NodeReference{ID: "end"}, Set: map[string]any{"action": "set"},
			},
		},
		{
			Type: OperationConnectNodes,
			ConnectNodes: &ConnectNodesOperation{
				Alias: "cycle", Source: NodeReference{ID: "end"}, Target: NodeReference{ID: "start"}, SourceHandle: "out",
			},
		},
	}, ApplyOptions{ExpectedCandidateChecksum: checksumForTest(t, baseCandidate)})
	if err == nil || result != nil || !strings.Contains(err.Error(), "ciclo sin un nodo wait") {
		t.Fatalf("result=%v err=%v", result, err)
	}
}

func TestNodePatchUsesLiteralDynamicMapKeys(t *testing.T) {
	node := map[string]any{
		"kind": "tool", "toolRef": "data_mutate",
		"args": map[string]any{"field.amount": "old", "keep": "yes"},
	}
	err := applyNodePatch(node, "tool", map[string]any{"args.field.amount": "new"}, []string{"args.keep"})
	if err != nil {
		t.Fatal(err)
	}
	args := node["args"].(map[string]any)
	if args["field.amount"] != "new" {
		t.Fatalf("args=%v", args)
	}
	if _, exists := args["keep"]; exists {
		t.Fatalf("args=%v", args)
	}
}

func TestApplyFlowOperationsAllowsIntermediateDisconnectedState(t *testing.T) {
	emptyFlow := json.RawMessage(`{
		"id": "empty", "name": "Vacio",
		"trigger": {"type": "message", "match": "any"},
		"nodes": [], "edges": []
	}`)
	emptyChecksum := checksumForTest(t, emptyFlow)

	// Paso intermedio: agregar un nodo agent sin conectar todavía al disparador
	operations := []FlowOperation{
		{
			Type: OperationAddNode,
			AddNode: &AddNodeOperation{
				Alias: "agent_1",
				Kind:  "agent",
				Set: map[string]any{
					"instruction": "Orienta al usuario y responde preguntas.",
					"outputs":     []string{"ok", "error"},
				},
			},
		},
	}

	result, err := ApplyFlowOperations(emptyFlow, operations, ApplyOptions{
		ExpectedCandidateChecksum: emptyChecksum,
	})
	if err != nil {
		t.Fatalf("ApplyFlowOperations debe permitir estados intermedios, pero falló con: %v", err)
	}

	if result == nil || len(result.Candidate) == 0 {
		t.Fatal("el resultado debe contener el nuevo candidato")
	}

	agentID := result.AliasToNodeID["agent_1"]
	if agentID == "" {
		t.Fatal("se esperaba id para agent_1")
	}

	// ValidateForAuthoring debe reportar diagnósticos que guíen a conectar el nodo
	report := ValidateForAuthoring(result.Candidate, AuthoringResourceSnapshot{})
	if !report.HasErrors() {
		t.Error("un nodo no conectado debe tener diagnósticos informativos")
	}
}
