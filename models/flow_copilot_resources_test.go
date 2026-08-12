package models

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFlowCopilotConnectionSafeNoPuedeSerializarSecretos(t *testing.T) {
	raw, err := json.Marshal(FlowCopilotConnectionSafe{
		Key: "store", Label: "Tienda", Driver: "meudim", Status: "active",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	serialized := strings.ToLower(string(raw))
	for _, forbidden := range []string{"credential", "baseurl", "mask", "lasterror"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("el DTO seguro expuso %q: %s", forbidden, serialized)
		}
	}
}
