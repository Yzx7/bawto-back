package main

import "testing"

func TestValidMethodKey(t *testing.T) {
	for _, valid := range []string{"yape", "plin_1", "bcp_cuenta"} {
		if !validMethodKey(valid) {
			t.Fatalf("clave válida rechazada: %s", valid)
		}
	}
	for _, invalid := range []string{"", "1yape", "Yape", "bcp-cuenta", "ámbito"} {
		if validMethodKey(invalid) {
			t.Fatalf("clave inválida aceptada: %s", invalid)
		}
	}
}
