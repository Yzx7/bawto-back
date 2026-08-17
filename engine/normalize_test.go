package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

// El caso que motivó todo: una IA escribe el grafo por MCP sin edges[].id.
// engine.Validate lo acepta —el motor no lee ese id— pero React Flow no dibuja
// ninguna conexión, así que el autor ve los bloques sueltos y cree que el flujo
// está roto cuando en realidad corre bien.
func TestNormalizeEdgeIDsRellenaLasAristasSinID(t *testing.T) {
	raw := []byte(`{
		"id":"flow_x","name":"X",
		"trigger":{"type":"message"},
		"nodes":[{"id":"n_agent","kind":"agent"}],
		"edges":[
			{"source":"trigger","target":"n_agent"},
			{"source":"n_agent","sourceHandle":"comprar","target":"n_compra"},
			{"source":"n_agent","sourceHandle":"soporte","target":"n_soporte"}
		]
	}`)

	out, err := NormalizeEdgeIDs(raw)
	if err != nil {
		t.Fatalf("normalizar: %v", err)
	}
	ids := edgeIDsOf(t, out)
	if len(ids) != 3 {
		t.Fatalf("se esperaban 3 aristas, hay %d", len(ids))
	}
	for i, id := range ids {
		if strings.TrimSpace(id) == "" {
			t.Fatalf("la arista %d sigue sin id", i)
		}
	}
	if ids[1] != "e_n_agent__comprar" || ids[2] != "e_n_agent__soporte" {
		t.Fatalf("el id no describe origen y rama: %v", ids)
	}
	if ids[0] != "e_trigger" {
		t.Fatalf("una arista sin rama debe nombrarse por su origen: %q", ids[0])
	}
}

// El checksum del borrador sostiene la concurrencia optimista de flow_put y la
// idempotencia de -publish-flow-file. Si el id fuera aleatorio, el mismo flujo
// lógico produciría un checksum distinto en cada escritura y expectedChecksum
// empezaría a fallar sin que nadie hubiera tocado el grafo.
func TestNormalizeEdgeIDsEsDeterminista(t *testing.T) {
	raw := []byte(`{"edges":[{"source":"a","target":"b"},{"source":"a","sourceHandle":"x","target":"c"}]}`)

	primera, err := NormalizeEdgeIDs(raw)
	if err != nil {
		t.Fatalf("primera: %v", err)
	}
	for i := 0; i < 5; i++ {
		otra, err := NormalizeEdgeIDs(raw)
		if err != nil {
			t.Fatalf("repetición %d: %v", i, err)
		}
		_, a, err := CanonicalChecksum(primera)
		if err != nil {
			t.Fatal(err)
		}
		_, b, err := CanonicalChecksum(otra)
		if err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Fatalf("checksum inestable entre pasadas: %s vs %s", a, b)
		}
	}
}

// Reescribir ids existentes cambiaría de golpe el checksum de todos los flujos
// vivos: el editor genera los suyos y los flujos guardados los traen.
func TestNormalizeEdgeIDsNoReescribeLosQueYaExisten(t *testing.T) {
	raw := []byte(`{"edges":[
		{"id":"e_1a2b3c4d","source":"a","target":"b"},
		{"source":"a","sourceHandle":"x","target":"c"}
	]}`)

	out, err := NormalizeEdgeIDs(raw)
	if err != nil {
		t.Fatalf("normalizar: %v", err)
	}
	ids := edgeIDsOf(t, out)
	if ids[0] != "e_1a2b3c4d" {
		t.Fatalf("se reescribió un id existente: %q", ids[0])
	}
	if ids[1] == "" {
		t.Fatal("no se rellenó el que faltaba")
	}
}

// Un documento que ya está completo tiene que salir byte a byte igual: si
// NormalizeEdgeIDs reserializara siempre, tocaría el checksum de cualquier
// flujo guardado la primera vez que se reescribiera sin cambios.
func TestNormalizeEdgeIDsNoTocaUnDocumentoCompleto(t *testing.T) {
	raw := []byte(`{"edges":[{"id":"e1","source":"a","target":"b"}],"pos":{"x":1}}`)

	out, err := NormalizeEdgeIDs(raw)
	if err != nil {
		t.Fatalf("normalizar: %v", err)
	}
	if string(out) != string(raw) {
		t.Fatalf("documento completo modificado:\n%s\n%s", raw, out)
	}
}

// Misma razón que Canonical: el documento sobrevive a un backend que no conoce
// todas sus claves. Perder `pos` al rellenar un id borraría la disposición que
// el dueño colocó a mano en el panel.
func TestNormalizeEdgeIDsConservaLasClavesDesconocidas(t *testing.T) {
	raw := []byte(`{
		"edges":[{"source":"a","target":"b","tag":"salto"}],
		"nodes":[{"id":"a","pos":{"x":10,"y":20},"futuro":"no lo pierdas"}],
		"extension":{"algo":1}
	}`)

	out, err := NormalizeEdgeIDs(raw)
	if err != nil {
		t.Fatalf("normalizar: %v", err)
	}
	for _, clave := range []string{"pos", "futuro", "extension", "tag"} {
		if !strings.Contains(string(out), clave) {
			t.Fatalf("se perdió la clave %q: %s", clave, out)
		}
	}
}

// Un id derivado no puede chocar con otro derivado —Validate ya exige que
// (source, sourceHandle) no se repita—, pero sí con uno explícito escrito a
// mano. El desempate existe para que la función sea total.
func TestNormalizeEdgeIDsDesempataContraUnIDExplicito(t *testing.T) {
	raw := []byte(`{"edges":[
		{"id":"e_a","source":"z","target":"b"},
		{"source":"a","target":"c"}
	]}`)

	out, err := NormalizeEdgeIDs(raw)
	if err != nil {
		t.Fatalf("normalizar: %v", err)
	}
	ids := edgeIDsOf(t, out)
	if ids[1] == ids[0] {
		t.Fatalf("colisión no resuelta: %v", ids)
	}
	if ids[1] != "e_a__2" {
		t.Fatalf("el desempate debe ser reproducible, no arbitrario: %q", ids[1])
	}
}

func edgeIDsOf(t *testing.T, raw []byte) []string {
	t.Helper()
	var doc struct {
		Edges []struct {
			ID string `json:"id"`
		} `json:"edges"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("no se pudo releer el documento: %v", err)
	}
	ids := make([]string, len(doc.Edges))
	for i, e := range doc.Edges {
		ids[i] = e.ID
	}
	return ids
}

// Un id repetido no se puede resolver solo: no hay forma de saber cuál de las
// dos aristas era la buena y React Flow dibuja una sola. Se avisa.
func TestValidateRechazaIDsDeAristaDuplicados(t *testing.T) {
	err := validateEdgeIDs([]Edge{
		{ID: "e1", Source: "a", Target: "b"},
		{ID: "e1", Source: "a", SourceHandle: "x", Target: "c"},
	})
	if err == nil {
		t.Fatal("un id de arista duplicado debía rechazarse")
	}
	if !strings.Contains(err.Error(), "duplicado") {
		t.Fatalf("el error no explica el problema: %v", err)
	}
}

// Y el que falta no es un error: es derivable, y exigirlo devolvería la
// contabilidad al autor, que es justo lo que este cambio quita.
func TestValidateToleraUnaAristaSinID(t *testing.T) {
	if err := validateEdgeIDs([]Edge{
		{Source: "a", Target: "b"},
		{Source: "a", SourceHandle: "x", Target: "c"},
	}); err != nil {
		t.Fatalf("una arista sin id no debía rechazarse: %v", err)
	}
}
