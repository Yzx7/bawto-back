package models

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Yzx7/sacs-chatbots/engine"
)

func clavesDe(t *testing.T, raw string) []string {
	t.Helper()
	var flow engine.Flow
	if err := json.Unmarshal([]byte(raw), &flow); err != nil {
		t.Fatal(err)
	}
	return flowObjectKeys(&flow)
}

// El aviso mira los dos sitios donde un grafo nombra una tabla: el bloque del
// grafo y la herramienta que se le ofrece a un agente. Si solo mirara el primero,
// un agente con `data_query` sobre una tabla borrada publicaría sin una palabra.
func TestClavesDeTablaDelGrafoIncluyenLasDelAgente(t *testing.T) {
	claves := clavesDe(t, `{
		"id": "f_obj", "name": "F",
		"trigger": {"type": "message", "match": "any"},
		"nodes": [
			{"id": "n1", "kind": "tool", "toolRef": "data_mutate", "args": {"object": "cobros"}},
			{"id": "n2", "kind": "agent", "tools": [{"ref": "data_query", "config": {"object": "servicios"}}]},
			{"id": "n3", "kind": "tool", "toolRef": "data_query", "args": {"object": "cobros"}},
			{"id": "n4", "kind": "send"}
		],
		"edges": []
	}`)
	if len(claves) != 2 || claves[0] != "cobros" || claves[1] != "servicios" {
		t.Fatalf("claves=%v; se esperaban cobros y servicios, sin repetir y ordenadas", claves)
	}
}

// El validador prohíbe interpolar el objeto —es lo que impide que el texto del
// cliente elija qué tabla se lee—, así que un marcador no es una tabla ausente:
// avisar de que no existe la tabla «{tabla_elegida}» sería ruido.
func TestClavesDeTablaIgnoranUnMarcador(t *testing.T) {
	claves := clavesDe(t, `{
		"id": "f_obj", "name": "F",
		"trigger": {"type": "message", "match": "any"},
		"nodes": [{"id": "n1", "kind": "tool", "toolRef": "data_query",
		           "args": {"object": "{tabla_elegida}"}}],
		"edges": []
	}`)
	if len(claves) != 0 {
		t.Fatalf("claves=%v; un marcador no es una tabla", claves)
	}
}

// La comprobación de verdad: una tabla sembrada no avisa y una ausente sí, con
// la clave dentro del mensaje. Necesita base porque lo que se prueba es la
// consulta contra `data_objects` de esa organización, no el recorrido del grafo.
func TestAvisoDeTablaAusenteAlPublicar(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "objwarn_")

	// `perfiles_contacto` la trae la organización al nacer; `citas` se decidió
	// deliberadamente no sembrarla, así que es el ejemplo honesto de tabla que el
	// grafo nombra y la organización no tiene.
	definition := json.RawMessage(`{
		"id": "f_obj", "name": "F",
		"trigger": {"type": "message", "match": "any"},
		"nodes": [
			{"id": "n1", "kind": "tool", "toolRef": "data_query", "args": {"object": "perfiles_contacto"}},
			{"id": "n2", "kind": "tool", "toolRef": "data_query", "args": {"object": "citas"}}
		],
		"edges": []
	}`)

	warnings, err := missingFlowObjectWarningsForBot(ctx, pool, bot.ID, definition)
	if err != nil {
		t.Fatalf("missingFlowObjectWarningsForBot: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("se esperaba un solo aviso, el de `citas`: %v", warnings)
	}
	if !strings.Contains(warnings[0], `"citas"`) || !strings.Contains(warnings[0], "rama de error") {
		t.Fatalf("el aviso debe nombrar la tabla y decir qué pasará: %q", warnings[0])
	}
}
