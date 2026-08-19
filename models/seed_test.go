package models

import (
	"encoding/json"
	"testing"

	"github.com/Yzx7/sacs-chatbots/db/defaults"
	"github.com/Yzx7/sacs-chatbots/engine"
)

// Lo que importa del grafo sembrado no es que la fila exista, sino que se pueda
// publicar: si `engine.Validate` lo rechaza, el dueño abre su bot recién creado y
// lo primero que encuentra es un flujo que no puede activar. Y se comprueba en
// las dos formas —núcleo pelado e injertado—, porque el injerto añade nodos y
// aristas y es justo donde puede romperse.
func TestGrafosSembradosSonPublicables(t *testing.T) {
	nucleo, err := defaults.FlujoInicial()
	if err != nil {
		t.Fatalf("defaults.FlujoInicial: %v", err)
	}
	injertado, err := defaults.Injertar(nucleo)
	if err != nil {
		t.Fatalf("defaults.Injertar: %v", err)
	}
	for _, caso := range []struct {
		nombre string
		grafo  json.RawMessage
	}{
		{"núcleo", nucleo},
		{"núcleo + fragmento comercial", injertado},
	} {
		t.Run(caso.nombre, func(t *testing.T) {
			// Se valida lo mismo que valida PublishFlow: el documento canónico, no
			// los bytes del fichero.
			definition, _, err := engine.CanonicalChecksum(caso.grafo)
			if err != nil {
				t.Fatalf("CanonicalChecksum: %v", err)
			}
			var parsed engine.Flow
			if err := json.Unmarshal(definition, &parsed); err != nil {
				t.Fatalf("el grafo sembrado no es JSON del editor: %v", err)
			}
			// El flujo se siembra como `message`, y PublishFlow rechaza publicar un
			// grafo cuyo trigger no coincida con el del flujo.
			if parsed.Trigger.Type != "message" {
				t.Fatalf("trigger %q: el flujo sembrado se crea como message", parsed.Trigger.Type)
			}
			if err := engine.Validate(&parsed); err != nil {
				t.Fatalf("engine.Validate rechaza el grafo sembrado: %v", err)
			}
		})
	}
}

// El injerto no sustituye el núcleo: le añade ramas. Si alguna vez perdiera
// nodos, un bot con tienda conectada nacería con menos conversación que uno sin
// ella y nadie lo notaría hasta verlo en producción.
func TestElInjertoNoPierdeNodosDelNucleo(t *testing.T) {
	nucleo, err := defaults.FlujoInicial()
	if err != nil {
		t.Fatalf("defaults.FlujoInicial: %v", err)
	}
	injertado, err := defaults.Injertar(nucleo)
	if err != nil {
		t.Fatalf("defaults.Injertar: %v", err)
	}
	delNucleo := idsDeNodos(t, nucleo)
	delInjerto := map[string]bool{}
	for _, id := range idsDeNodos(t, injertado) {
		delInjerto[id] = true
	}
	for _, id := range delNucleo {
		if !delInjerto[id] {
			t.Errorf("el injerto perdió el nodo %q del núcleo", id)
		}
	}
	if len(delInjerto) <= len(delNucleo) {
		t.Fatalf("el injerto debe añadir nodos: núcleo=%d injertado=%d", len(delNucleo), len(delInjerto))
	}
}

func idsDeNodos(t *testing.T, grafo json.RawMessage) []string {
	t.Helper()
	var doc struct {
		Nodes []struct {
			ID string `json:"id"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(grafo, &doc); err != nil {
		t.Fatalf("grafo ilegible: %v", err)
	}
	ids := make([]string, 0, len(doc.Nodes))
	for _, node := range doc.Nodes {
		ids = append(ids, node.ID)
	}
	return ids
}
