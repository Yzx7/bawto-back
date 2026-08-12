package authoring

import (
	"strings"
	"testing"
)

func TestValidateAuthoringContextChecksTenantBindings(t *testing.T) {
	raw := []byte(`{
      "id":"bindings","name":"Bindings","trigger":{"type":"message","match":"any"},
      "nodes":[
        {"id":"write","kind":"tool","toolRef":"data_mutate","args":{
          "object":"orders","operation":"create","idempotencyKey":"order:{input.id}",
          "field.total":"not-a-number","field.missing":"x"
        }},
        {"id":"catalog","kind":"tool","toolRef":"catalog_search","args":{"connection":"shop","query":"{input.text}"}},
        {"id":"template","kind":"send","templateName":"welcome","templateLanguage":"es"}
      ],
      "edges":[]
    }`)
	resources := AuthoringResourceSnapshot{
		DataObjects: []DataObjectResource{{Key: "orders", Fields: []DataFieldResource{
			{Key: "total", Type: "number", Required: true},
			{Key: "email", Type: "string", Required: true},
		}}},
		Connections: []ConnectionResource{{Key: "shop", Status: "disabled", ToolRefs: []string{"catalog_search"}}},
		Templates:   []TemplateResource{{Name: "welcome", Language: "es", Status: "pending"}},
	}
	diagnostics := ValidateAuthoringContext(raw, resources)
	wantCodes := map[string]bool{
		"data_field_type_mismatch":    false,
		"unknown_data_field":          false,
		"missing_required_data_field": false,
		"connection_inactive":         false,
		"template_not_approved":       false,
	}
	for _, diagnostic := range diagnostics {
		if _, wanted := wantCodes[diagnostic.Code]; wanted {
			wantCodes[diagnostic.Code] = true
		}
	}
	for code, found := range wantCodes {
		if !found {
			t.Fatalf("falta %s en %+v", code, diagnostics)
		}
	}
}

func TestValidateAuthoringContextTracksDefiniteMaybeAndUndefinedVariables(t *testing.T) {
	raw := []byte(`{
      "id":"variables","name":"Variables","trigger":{"type":"message","match":"any"},
      "nodes":[
        {"id":"branch","kind":"condition","expression":"input.text == yes"},
        {"id":"produce","kind":"action","action":"set","params":{"sale":"{input.text}"}},
        {"id":"bypass","kind":"send","body":"Sin dato"},
        {"id":"use","kind":"send","body":"{sale.id} / {never.value} / {input.text}"},
        {"id":"end","kind":"action","action":"end"}
      ],
      "edges":[
        {"id":"e0","source":"trigger","target":"branch"},
        {"id":"e1","source":"branch","target":"produce","sourceHandle":"true"},
        {"id":"e2","source":"branch","target":"bypass","sourceHandle":"false"},
        {"id":"e3","source":"produce","target":"use"},
        {"id":"e4","source":"bypass","target":"use"},
        {"id":"e5","source":"use","target":"end"}
      ]
    }`)
	diagnostics := ValidateAuthoringContext(raw, AuthoringResourceSnapshot{})
	var maybeSale, undefinedNever bool
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "maybe_undefined_variable" && strings.Contains(diagnostic.Message, "sale") {
			maybeSale = true
		}
		if diagnostic.Code == "undefined_variable" && strings.Contains(diagnostic.Message, "never") {
			undefinedNever = true
		}
		if strings.Contains(diagnostic.Message, "input") {
			t.Fatalf("input built-in marcado como problema: %+v", diagnostic)
		}
	}
	if !maybeSale || !undefinedNever {
		t.Fatalf("diagnostics=%+v", diagnostics)
	}
}

func TestValidateAuthoringContextRejectsSaveAsCollisions(t *testing.T) {
	raw := []byte(`{
      "id":"collision","name":"Collision","trigger":{"type":"message","match":"any"},
      "nodes":[
        {"id":"one","kind":"wait","saveAs":"reply"},
        {"id":"two","kind":"wait","saveAs":"reply"}
      ],
      "edges":[]
    }`)
	diagnostics := ValidateAuthoringContext(raw, AuthoringResourceSnapshot{})
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "save_as_collision" {
			return
		}
	}
	t.Fatalf("diagnostics=%+v", diagnostics)
}

func TestLintCandidateFindsErrorIdempotencyAndDoubleResponseRisks(t *testing.T) {
	raw := []byte(`{
      "id":"lint","name":"Lint","trigger":{"type":"message","match":"any"},
      "nodes":[
        {"id":"write","kind":"tool","toolRef":"data_mutate","args":{"idempotencyKey":"fixed"}},
        {"id":"agent","kind":"agent","outputs":["ok"],"replyOn":["ok"]},
        {"id":"send","kind":"send","body":"Duplicado"},
        {"id":"condition","kind":"condition","expression":"input.text"}
      ],
      "edges":[
        {"id":"e1","source":"write","target":"condition","sourceHandle":"error"},
        {"id":"e2","source":"agent","target":"send","sourceHandle":"ok"}
      ]
    }`)
	diagnostics := LintCandidate(raw)
	want := map[string]bool{
		"unhandled_tool_error":     false,
		"unstable_idempotency_key": false,
		"double_response_branch":   false,
	}
	for _, diagnostic := range diagnostics {
		if _, exists := want[diagnostic.Code]; exists {
			want[diagnostic.Code] = true
		}
		if diagnostic.Severity != SeverityWarning || diagnostic.Source != SourceLint {
			t.Fatalf("lint bloqueante o fuente incorrecta: %+v", diagnostic)
		}
	}
	for code, found := range want {
		if !found {
			t.Fatalf("falta %s en %+v", code, diagnostics)
		}
	}
}
