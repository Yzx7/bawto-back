package authoring

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

type FieldChangeType string

const (
	FieldSet     FieldChangeType = "set"
	FieldUnset   FieldChangeType = "unset"
	FieldChanged FieldChangeType = "changed"
)

// FieldChange is a leaf-level map change. Arrays are intentionally treated as
// one semantic value because their order is meaningful for outputs and cases.
type FieldChange struct {
	Path   string          `json:"path"`
	Type   FieldChangeType `json:"type"`
	Before any             `json:"before,omitempty"`
	After  any             `json:"after,omitempty"`
}

type EntityChangeType string

const (
	EntityAdded    EntityChangeType = "added"
	EntityRemoved  EntityChangeType = "removed"
	EntityModified EntityChangeType = "modified"
)

// EntityDiff matches nodes and edges by id, never by array position.
type EntityDiff struct {
	ID     string           `json:"id"`
	Type   EntityChangeType `json:"type"`
	Before map[string]any   `json:"before,omitempty"`
	After  map[string]any   `json:"after,omitempty"`
	Fields []FieldChange    `json:"fields,omitempty"`
}

type FlowDiff struct {
	Flow    []FieldChange `json:"flow,omitempty"`
	Trigger []FieldChange `json:"trigger,omitempty"`
	Nodes   []EntityDiff  `json:"nodes,omitempty"`
	Edges   []EntityDiff  `json:"edges,omitempty"`
}

func (diff FlowDiff) Empty() bool {
	return len(diff.Flow) == 0 && len(diff.Trigger) == 0 && len(diff.Nodes) == 0 && len(diff.Edges) == 0
}

// SemanticDiff ignores node/edge array order and compares their generic JSON
// objects by id. Unknown fields participate just like known ones.
func SemanticDiff(beforeRaw, afterRaw []byte) (FlowDiff, error) {
	before, err := ParseDocument(beforeRaw)
	if err != nil {
		return FlowDiff{}, fmt.Errorf("documento anterior: %w", err)
	}
	after, err := ParseDocument(afterRaw)
	if err != nil {
		return FlowDiff{}, fmt.Errorf("documento posterior: %w", err)
	}
	if err := validateDocumentIdentity(before); err != nil {
		return FlowDiff{}, fmt.Errorf("documento anterior: %w", err)
	}
	if err := validateDocumentIdentity(after); err != nil {
		return FlowDiff{}, fmt.Errorf("documento posterior: %w", err)
	}

	beforeTrigger, _ := objectField(before, "trigger")
	afterTrigger, _ := objectField(after, "trigger")
	beforeNodes, _ := objectArrayField(before, "nodes")
	afterNodes, _ := objectArrayField(after, "nodes")
	beforeEdges, _ := objectArrayField(before, "edges")
	afterEdges, _ := objectArrayField(after, "edges")

	diff := FlowDiff{
		Flow:    compareMaps(withoutKeys(before, "trigger", "nodes", "edges"), withoutKeys(after, "trigger", "nodes", "edges"), ""),
		Trigger: compareMaps(beforeTrigger, afterTrigger, ""),
	}
	diff.Nodes, err = compareEntities(beforeNodes, afterNodes)
	if err != nil {
		return FlowDiff{}, err
	}
	diff.Edges, err = compareEntities(beforeEdges, afterEdges)
	if err != nil {
		return FlowDiff{}, err
	}
	return diff, nil
}

func compareEntities(beforeItems, afterItems []map[string]any) ([]EntityDiff, error) {
	before, err := indexByID(beforeItems, "entidad")
	if err != nil {
		return nil, err
	}
	after, err := indexByID(afterItems, "entidad")
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(before)+len(after))
	seen := make(map[string]bool, len(before)+len(after))
	for id := range before {
		ids = append(ids, id)
		seen[id] = true
	}
	for id := range after {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	result := make([]EntityDiff, 0)
	for _, id := range ids {
		beforeEntity, existedBefore := before[id]
		afterEntity, existsAfter := after[id]
		switch {
		case !existedBefore:
			result = append(result, EntityDiff{ID: id, Type: EntityAdded, After: afterEntity})
		case !existsAfter:
			result = append(result, EntityDiff{ID: id, Type: EntityRemoved, Before: beforeEntity})
		default:
			fields := compareMaps(withoutKeys(beforeEntity, "id"), withoutKeys(afterEntity, "id"), "")
			if len(fields) > 0 {
				result = append(result, EntityDiff{ID: id, Type: EntityModified, Fields: fields})
			}
		}
	}
	return result, nil
}

func compareMaps(before, after map[string]any, prefix string) []FieldChange {
	keys := make([]string, 0, len(before)+len(after))
	seen := make(map[string]bool, len(before)+len(after))
	for key := range before {
		keys = append(keys, key)
		seen[key] = true
	}
	for key := range after {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	result := make([]FieldChange, 0)
	for _, key := range keys {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		beforeValue, existedBefore := before[key]
		afterValue, existsAfter := after[key]
		switch {
		case !existedBefore:
			result = append(result, FieldChange{Path: path, Type: FieldSet, After: afterValue})
		case !existsAfter:
			result = append(result, FieldChange{Path: path, Type: FieldUnset, Before: beforeValue})
		default:
			beforeObject, beforeIsObject := beforeValue.(map[string]any)
			afterObject, afterIsObject := afterValue.(map[string]any)
			if beforeIsObject && afterIsObject {
				result = append(result, compareMaps(beforeObject, afterObject, path)...)
				continue
			}
			if !jsonValuesEqual(beforeValue, afterValue) {
				result = append(result, FieldChange{Path: path, Type: FieldChanged, Before: beforeValue, After: afterValue})
			}
		}
	}
	return result
}

func withoutKeys(source map[string]any, keys ...string) map[string]any {
	excluded := make(map[string]bool, len(keys))
	for _, key := range keys {
		excluded[key] = true
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		if !excluded[key] {
			result[key] = value
		}
	}
	return result
}

func jsonValuesEqual(left, right any) bool {
	leftNumber, leftIsNumber := left.(json.Number)
	rightNumber, rightIsNumber := right.(json.Number)
	if leftIsNumber || rightIsNumber {
		return leftIsNumber && rightIsNumber && leftNumber.String() == rightNumber.String()
	}
	return reflect.DeepEqual(left, right)
}
