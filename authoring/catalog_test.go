package authoring

import (
	"reflect"
	"sort"
	"testing"

	enginetools "github.com/Yzx7/sacs-chatbots/engine/tools"
)

func TestNodeCatalogCoversEngineKindsAndPorts(t *testing.T) {
	wantKinds := []string{"action", "agent", "condition", "router", "send", "tool", "wait"}
	catalog := NodeCatalog()
	gotKinds := make([]string, 0, len(catalog))
	for _, spec := range catalog {
		gotKinds = append(gotKinds, spec.Kind)
		if len(spec.Fields) == 0 {
			t.Fatalf("%s no declara campos", spec.Kind)
		}
	}
	sort.Strings(gotKinds)
	if !reflect.DeepEqual(gotKinds, wantKinds) {
		t.Fatalf("kinds=%v want=%v", gotKinds, wantKinds)
	}

	agent := map[string]any{"kind": "agent", "outputs": []any{"sale", "support"}}
	ports, err := ResolveOutputPorts(agent)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{ports[0].Handle, ports[1].Handle}; !reflect.DeepEqual(got, []string{"sale", "support"}) {
		t.Fatalf("agent ports=%v", got)
	}
	router := map[string]any{"kind": "router", "cases": []any{
		map[string]any{"id": "known"}, map[string]any{"id": "unknown"},
	}}
	ports, err = ResolveOutputPorts(router)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{ports[0].ID, ports[1].ID, ports[2].ID}; !reflect.DeepEqual(got, []string{"known", "unknown", "default"}) {
		t.Fatalf("router ports=%v", got)
	}
	wait := map[string]any{"kind": "wait", "accepts": []any{"text", "image"}}
	ports, err = ResolveOutputPorts(wait)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{ports[0].ID, ports[1].ID}; !reflect.DeepEqual(got, []string{"text", "image"}) {
		t.Fatalf("wait ports=%v", got)
	}
	actionEnd := map[string]any{"kind": "action", "action": "end"}
	ports, err = ResolveOutputPorts(actionEnd)
	if err != nil || len(ports) != 0 {
		t.Fatalf("end ports=%v err=%v", ports, err)
	}
}

func TestNodeCatalogReturnsDetachedValues(t *testing.T) {
	first := NodeCatalog()
	first[0].Defaults["injected"] = true
	first[0].Fields[0].Label = "mutated"
	second := NodeCatalog()
	if _, exists := second[0].Defaults["injected"]; exists {
		t.Fatal("defaults compartidos con el llamador")
	}
	if second[0].Fields[0].Label == "mutated" {
		t.Fatal("fields compartidos con el llamador")
	}
}

func TestRuntimeToolCatalogIsDerivedFromEngineRegistry(t *testing.T) {
	want := make(map[string]enginetools.Spec)
	for _, spec := range enginetools.ForAgent() {
		want[spec.Name] = spec
	}
	for _, spec := range enginetools.ForGraph() {
		want[spec.Name] = spec
	}
	got := RuntimeToolCatalog()
	if len(got) != len(want) {
		t.Fatalf("tools=%d want=%d", len(got), len(want))
	}
	for index, spec := range got {
		if index > 0 && got[index-1].Name >= spec.Name {
			t.Fatalf("catálogo no ordenado: %s antes de %s", got[index-1].Name, spec.Name)
		}
		source, exists := want[spec.Name]
		if !exists {
			t.Fatalf("tool inventada %q", spec.Name)
		}
		if spec.ForAgent != source.ForAgent || spec.ForGraph != source.ForGraph || spec.Effect != string(source.Effect) {
			t.Fatalf("proyección divergente para %s: %+v source=%+v", spec.Name, spec, source)
		}
	}
	if len(CatalogHash()) != 64 || CatalogHash() != CatalogHash() {
		t.Fatalf("hash de catálogo inválido %q", CatalogHash())
	}
}
