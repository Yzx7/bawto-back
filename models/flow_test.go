package models

import "testing"

func TestValidFlowKey(t *testing.T) {
	ok := []string{"atencion", "recordatorio-d3", "flow_waa_isp", "a"}
	bad := []string{"", "Atencion", "3dias", "-x", "con espacio", "acentuación"}
	for _, k := range ok {
		if !ValidFlowKey(k) {
			t.Fatalf("%q debería ser válida", k)
		}
	}
	for _, k := range bad {
		if ValidFlowKey(k) {
			t.Fatalf("%q no debería ser válida", k)
		}
	}
}

// La key del backfill se deriva del propio grafo: es lo que hace que
// re-ejecutarlo no duplique flujos.
func TestFlowKeyFromDefinition(t *testing.T) {
	cases := []struct{ flowID, fallback, want string }{
		{"flow_waa_isp", "WAA", "flow_waa_isp"},
		{"", "WAA — Atención integral ISP", "waa-atencin-integral-isp"},
		{"", "", "principal"},
		{"123", "", "principal"},
		{"Recordatorio D-3", "", "recordatorio-d-3"},
	}
	for _, c := range cases {
		if got := FlowKeyFromDefinition(c.flowID, c.fallback); got != c.want {
			t.Fatalf("FlowKeyFromDefinition(%q,%q) = %q, se esperaba %q", c.flowID, c.fallback, got, c.want)
		}
	}
	// Sea cual sea la entrada, el resultado tiene que pasar el CHECK de la tabla.
	for _, in := range []string{"", "###", "ÑOÑO", "x" + string(make([]byte, 200))} {
		if k := FlowKeyFromDefinition(in, ""); !ValidFlowKey(k) {
			t.Fatalf("key derivada inválida para %q: %q", in, k)
		}
	}
}
