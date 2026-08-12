package authoring

import (
	"strings"
	"testing"
)

func TestSemanticDiffMatchesEntitiesByIDNotArrayOrder(t *testing.T) {
	after := []byte(`{
      "edges": [
        {"target":"end","source":"start","id":"e-start-end","editorMeta":{"keep":true},"label":"visible"},
        {"target":"start","source":"trigger","id":"e-trigger-start","editorMeta":"root-edge"}
      ],
      "nodes": [
        {"editorMeta":{"keep":"end"},"action":"end","pos":{"y":20,"x":400},"kind":"action","id":"end"},
        {"editorMeta":{"keep":"node","collapsed":false},"templateLanguage":"es","templateName":"legacy","body":"Hola","pos":{"y":20,"x":10},"kind":"send","id":"start"}
      ],
      "trigger":{"editorHint":"keep-trigger","match":"any","type":"message"},
      "editorExtension":{"nested":[1,{"keep":true}],"large":9007199254740993},
      "name":"Flujo base","id":"flow-base"
    }`)
	diff, err := SemanticDiff(baseCandidate, after)
	if err != nil {
		t.Fatal(err)
	}
	if !diff.Empty() {
		t.Fatalf("reordenar entidades produjo diff: %+v", diff)
	}
}

func TestSemanticDiffReportsUnknownNestedFieldByID(t *testing.T) {
	after := []byte(strings.Replace(string(baseCandidate), `"collapsed": false`, `"collapsed": true`, 1))
	diff, err := SemanticDiff(baseCandidate, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Nodes) != 1 || diff.Nodes[0].ID != "start" || len(diff.Nodes[0].Fields) != 1 {
		t.Fatalf("diff=%+v", diff)
	}
	change := diff.Nodes[0].Fields[0]
	if change.Path != "editorMeta.collapsed" || change.Type != FieldChanged {
		t.Fatalf("change=%+v", change)
	}
}
