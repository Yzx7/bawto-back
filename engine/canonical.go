package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Canonical normaliza el JSON de un grafo para poder compararlo: reordena las
// claves de cada objeto y elimina el espaciado. Dos guardados del mismo flujo con
// las claves en distinto orden producen el mismo resultado, así que publicar dos
// veces el mismo grafo se detecta como no-op (§5.2 del plan).
//
// Trabaja sobre el documento genérico, no sobre engine.Flow: el JSON del editor
// lleva campos que el motor no usa (`pos` de cada nodo, entre otros) y pasarlo
// por la struct los borraría, destruyendo el layout al restaurar una versión.
func Canonical(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// UseNumber conserva el literal del número: sin esto 1e9 o un entero grande
	// vuelven como float64 y el checksum cambiaría sin que cambie el flujo.
	dec.UseNumber()

	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("flujo inválido (no es JSON): %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("flujo inválido: hay contenido después del documento JSON")
	}
	// encoding/json ordena las claves de un map al serializar, que es
	// exactamente la normalización que hace falta.
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("no se pudo normalizar el flujo: %w", err)
	}
	return out, nil
}

// Checksum identifica una definición ya normalizada por Canonical.
func Checksum(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// CanonicalChecksum normaliza y resume en un paso.
func CanonicalChecksum(raw []byte) ([]byte, string, error) {
	canonical, err := Canonical(raw)
	if err != nil {
		return nil, "", err
	}
	return canonical, Checksum(canonical), nil
}
