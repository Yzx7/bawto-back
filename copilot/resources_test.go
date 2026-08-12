package copilot

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Yzx7/sacs-chatbots/authoring"
	runtimetools "github.com/Yzx7/sacs-chatbots/engine/tools"
)

func TestConnectionAuthoringCapabilitiesMeudim(t *testing.T) {
	capabilities, tools := connectionAuthoringCapabilities("meudim")
	if !reflect.DeepEqual(capabilities, []string{"catalog_read", "order_write", "manual_payment"}) {
		t.Fatalf("capabilities inesperadas: %v", capabilities)
	}
	wantTools := []string{"catalog_product", "catalog_search", "order_create", "payment_intent_create", "payment_submit"}
	if !reflect.DeepEqual(tools, wantTools) {
		t.Fatalf("tools inesperadas: %v", tools)
	}
	for _, ref := range tools {
		spec := runtimetools.Get(ref)
		if spec == nil || !spec.ForGraph {
			t.Fatalf("capability %q derivó hacia una tool inexistente/no configurable en grafo", ref)
		}
	}
}

func TestSafeConnectionViewExponeSoloAllowlist(t *testing.T) {
	raw, err := json.Marshal(SafeConnectionView{
		Key: "store", Label: "Tienda", Driver: "meudim",
		Capabilities: []string{"catalog_read"}, Status: "active",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	serialized := strings.ToLower(string(raw))
	for _, forbidden := range []string{"credential", "baseurl", "mask", "lasterror"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("la salida segura expuso %q: %s", forbidden, serialized)
		}
	}
}

func TestResourceSnapshotHashEsDeterminista(t *testing.T) {
	snapshot := authoring.AuthoringResourceSnapshot{
		DataObjects: []authoring.DataObjectResource{{
			Key: "orders", Fields: []authoring.DataFieldResource{{Key: "status", Type: "text"}},
		}},
	}
	first, err := resourceSnapshotHash(snapshot)
	if err != nil {
		t.Fatalf("primer hash: %v", err)
	}
	second, err := resourceSnapshotHash(snapshot)
	if err != nil {
		t.Fatalf("segundo hash: %v", err)
	}
	if first == "" || first != second {
		t.Fatalf("hash no determinista: %q != %q", first, second)
	}
}
