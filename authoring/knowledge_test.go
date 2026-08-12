package authoring

import (
	"embed"
	"reflect"
	"sort"
	"strings"
	"testing"
)

//go:embed knowledge/goldens/*.json
var goldenFlowFiles embed.FS

func TestBundledPlaybooksAreVersionedValidAndTransitivelyHashed(t *testing.T) {
	summaries := ListPlaybooks()
	if len(summaries) != 3 {
		t.Fatalf("playbooks=%d: %+v", len(summaries), summaries)
	}
	ids := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		ids = append(ids, summary.ID)
		if summary.Version != "1.0.0" || len(summary.BundleHash) != 64 {
			t.Fatalf("summary inválido: %+v", summary)
		}
		bundle, exists := GetPlaybook(summary.ID, summary.Version)
		if !exists {
			t.Fatalf("no se pudo leer %s@%s", summary.ID, summary.Version)
		}
		if err := ValidatePlaybook(bundle.Playbook); err != nil {
			t.Fatalf("%s: %v", summary.ID, err)
		}
		if bundle.BundleHash != summary.BundleHash || bundle.CatalogHash != CatalogHash() {
			t.Fatalf("bundle divergente para %s", summary.ID)
		}
		if len(bundle.Patterns) == 0 || len(bundle.Policies) == 0 || len(bundle.Checks) == 0 {
			t.Fatalf("bundle no transitivo para %s: %+v", summary.ID, bundle)
		}
		latest, exists := GetPlaybook(summary.ID, "")
		if !exists || latest.BundleHash != bundle.BundleHash {
			t.Fatalf("latest no resolvió %s", summary.ID)
		}
	}
	sort.Strings(ids)
	want := []string{"catalog_order_manual_payment", "payment_receipt_capture", "support_triage"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids=%v want=%v", ids, want)
	}
}

func TestKnowledgeBundleHashChangesWithTransitiveContent(t *testing.T) {
	bundle, exists := GetPlaybook("payment_receipt_capture", "1.0.0")
	if !exists {
		t.Fatal("playbook no encontrado")
	}
	changed := bundle.Playbook
	changed.Invariants = append(append([]string(nil), changed.Invariants...), "nueva regla")
	changedBundle := buildKnowledgeBundle(changed)
	if changedBundle.BundleHash == bundle.BundleHash {
		t.Fatal("el hash ignoró contenido del playbook")
	}
}

func TestValidatePlaybookRejectsCatalogDrift(t *testing.T) {
	bundle, _ := GetPlaybook("support_triage", "1.0.0")
	broken := bundle.Playbook
	broken.RequiredCapabilities = append(cloneCapabilities(broken.RequiredCapabilities), CapabilityRequirement{RuntimeTools: []string{"invented_tool"}})
	err := ValidatePlaybook(broken)
	if err == nil || !strings.Contains(err.Error(), "invented_tool") {
		t.Fatalf("err=%v", err)
	}
}

func TestPlaybookGoldensPassEngineBindingsAndLint(t *testing.T) {
	resources := AuthoringResourceSnapshot{
		DataObjects: []DataObjectResource{{
			Key: "payments",
			Fields: []DataFieldResource{
				{Key: "operationCode", Type: "string", Required: true},
				{Key: "status", Type: "string", Required: true},
			},
		}},
		Connections: []ConnectionResource{{
			Key: "shop", Driver: "store", Status: "active",
			ToolRefs: []string{"catalog_search", "catalog_product", "order_create", "payment_intent_create", "payment_submit"},
		}},
	}
	entries, err := goldenFlowFiles.ReadDir("knowledge/goldens")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("goldens=%d", len(entries))
	}
	for _, entry := range entries {
		t.Run(entry.Name(), func(t *testing.T) {
			raw, err := goldenFlowFiles.ReadFile("knowledge/goldens/" + entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			report := ValidateForAuthoring(raw, resources)
			if report.HasErrors() {
				t.Fatalf("errores: %+v", report.Diagnostics)
			}
			for _, diagnostic := range report.Diagnostics {
				if diagnostic.Source == SourceLint {
					t.Fatalf("golden con lint: %+v", diagnostic)
				}
			}
		})
	}
}
