package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// NormalizeEdgeIDs rellena `edges[].id` cuando falta o viene vacío.
//
// El motor nunca lee ese id: enruta por source/sourceHandle/target. Pero React
// Flow lo exige para dibujar, así que una arista sin id produce un grafo que
// valida, corre bien y **no se ve** en el editor. Pedirle ese id a quien escribe
// el flujo —una IA por MCP, el Copilot— es pedirle una contabilidad que no
// aporta información y que, cuando se olvida, falla en silencio. Se calcula.
//
// Tres propiedades que no son opcionales:
//
//   - **Determinista.** `draft` se guarda ya canonizado y su checksum sostiene
//     la concurrencia optimista de flow_put y la idempotencia de
//     -publish-flow-file. Un id aleatorio haría que el mismo flujo lógico
//     produjera un checksum distinto en cada escritura y `expectedChecksum`
//     empezaría a fallar sin que nadie hubiera tocado el grafo.
//   - **Solo rellena; nunca reescribe un id existente.** El editor genera los
//     suyos y los flujos guardados los traen. Reescribirlos cambiaría de golpe
//     el checksum de todos los flujos vivos.
//   - **Sobre el documento genérico, no sobre la struct.** Igual que Canonical:
//     así sobreviven `pos` y cualquier clave que este backend no conozca.
//
// La unicidad sale gratis del propio validador: `Validate` ya exige que
// (source, sourceHandle) no se repita, así que un id derivado de ese par no
// puede colisionar con otro derivado. El desempate solo existe por si choca con
// un id explícito escrito a mano.
func NormalizeEdgeIDs(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("flujo inválido (no es JSON): %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("flujo inválido: hay contenido después del documento JSON")
	}

	root, ok := doc.(map[string]any)
	if !ok {
		return raw, nil
	}
	edges, ok := root["edges"].([]any)
	if !ok {
		return raw, nil
	}

	// Primera pasada: qué ids ya están ocupados. Incluye los explícitos, que se
	// respetan tal cual.
	taken := make(map[string]struct{}, len(edges))
	for _, item := range edges {
		edge, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := edge["id"].(string); ok && id != "" {
			taken[id] = struct{}{}
		}
	}

	// Segunda pasada: rellenar los que falten, en orden del array para que el
	// desempate sea reproducible.
	changed := false
	for _, item := range edges {
		edge, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := edge["id"].(string); ok && id != "" {
			continue
		}
		source, _ := edge["source"].(string)
		handle, _ := edge["sourceHandle"].(string)
		id := uniqueEdgeID(derivedEdgeID(source, handle), taken)
		edge["id"] = id
		taken[id] = struct{}{}
		changed = true
	}
	if !changed {
		return raw, nil
	}

	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("no se pudo normalizar el flujo: %w", err)
	}
	return out, nil
}

// derivedEdgeID nombra la arista por lo que la identifica de verdad para el
// motor: de dónde sale y por qué rama.
func derivedEdgeID(source, handle string) string {
	if source == "" {
		source = "edge"
	}
	if handle == "" {
		return "e_" + source
	}
	return "e_" + source + "__" + handle
}

func uniqueEdgeID(candidate string, taken map[string]struct{}) string {
	if _, clash := taken[candidate]; !clash {
		return candidate
	}
	for n := 2; ; n++ {
		next := candidate + "__" + strconv.Itoa(n)
		if _, clash := taken[next]; !clash {
			return next
		}
	}
}
