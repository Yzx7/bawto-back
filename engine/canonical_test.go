package engine

import (
	"strings"
	"testing"
)

// El mismo grafo con las claves en otro orden debe dar el mismo checksum: de eso
// depende que republicar sin cambios se detecte como no-op (§5.2).
func TestCanonicalIgnoraOrdenDeClaves(t *testing.T) {
	a := []byte(`{"id":"f1","name":"F","trigger":{"type":"message","match":"any"},"nodes":[],"edges":[]}`)
	b := []byte(`{"edges":[],"nodes":[],"trigger":{"match":"any","type":"message"},"name":"F","id":"f1"}`)

	_, sumA, err := CanonicalChecksum(a)
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	_, sumB, err := CanonicalChecksum(b)
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if sumA != sumB {
		t.Fatalf("mismo grafo, checksums distintos: %s vs %s", sumA, sumB)
	}
}

// El espaciado tampoco cuenta, pero un cambio real sí.
func TestCanonicalDetectaCambioReal(t *testing.T) {
	a := []byte(`{"id":"f1","name":"F","nodes":[{"id":"n1","kind":"send","body":"hola"}]}`)
	spaced := []byte("{\n  \"id\": \"f1\",\n  \"name\": \"F\",\n  \"nodes\": [ { \"id\": \"n1\", \"kind\": \"send\", \"body\": \"hola\" } ]\n}")
	changed := []byte(`{"id":"f1","name":"F","nodes":[{"id":"n1","kind":"send","body":"hola!"}]}`)

	_, sumA, _ := CanonicalChecksum(a)
	_, sumSpaced, _ := CanonicalChecksum(spaced)
	_, sumChanged, _ := CanonicalChecksum(changed)

	if sumA != sumSpaced {
		t.Fatal("el espaciado no debería cambiar el checksum")
	}
	if sumA == sumChanged {
		t.Fatal("cambiar el texto de un nodo debe cambiar el checksum")
	}
}

// El JSON del editor lleva campos que el motor no modela (`pos`). Normalizar no
// puede borrarlos: restaurar una versión perdería el layout del canvas.
func TestCanonicalConservaCamposDesconocidos(t *testing.T) {
	raw := []byte(`{"id":"f1","nodes":[{"id":"n1","kind":"send","pos":{"x":20,"y":80},"body":"hola"}]}`)
	canonical, _, err := CanonicalChecksum(raw)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	got := string(canonical)
	for _, want := range []string{`"pos"`, `"x":20`, `"y":80`} {
		if !strings.Contains(got, want) {
			t.Fatalf("se perdió %s al normalizar: %s", want, got)
		}
	}
}

// Los enteros no pueden volver como 2e+01: eso cambiaría el checksum sin que
// nadie tocara el flujo.
func TestCanonicalConservaLiteralNumerico(t *testing.T) {
	raw := []byte(`{"a":20,"b":1000000000000000000,"c":1.5}`)
	canonical, _, err := CanonicalChecksum(raw)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	if got := string(canonical); got != `{"a":20,"b":1000000000000000000,"c":1.5}` {
		t.Fatalf("números alterados: %s", got)
	}
}

func TestCanonicalRechazaJSONInvalido(t *testing.T) {
	if _, _, err := CanonicalChecksum([]byte(`{"id":`)); err == nil {
		t.Fatal("se esperaba error con JSON incompleto")
	}
	if _, _, err := CanonicalChecksum([]byte(`{} {}`)); err == nil {
		t.Fatal("se esperaba error con dos documentos")
	}
}
