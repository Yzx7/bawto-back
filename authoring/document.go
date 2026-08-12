package authoring

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Document is the generic JSON representation edited by the authoring layer.
// It deliberately does not embed engine.Flow: editor-only fields and future
// extensions must survive an authoring operation unchanged.
type Document map[string]any

// ParseDocument decodes exactly one JSON object while preserving numeric
// literals through json.Number.
func ParseDocument(raw []byte) (Document, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var document Document
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("documento de flujo inválido: %w", err)
	}
	if document == nil {
		return nil, fmt.Errorf("el documento de flujo debe ser un objeto JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("hay contenido después del documento de flujo")
		}
		return nil, fmt.Errorf("contenido extra inválido: %w", err)
	}
	return document, nil
}

// MarshalDocument serializes the generic document. encoding/json sorts object
// keys, which also makes operation results deterministic.
func MarshalDocument(document Document) ([]byte, error) {
	if document == nil {
		return nil, fmt.Errorf("el documento de flujo no puede ser nil")
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("no se pudo serializar el documento de flujo: %w", err)
	}
	return raw, nil
}

func cloneJSONValue(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("el valor no es JSON válido: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var clone any
	if err := decoder.Decode(&clone); err != nil {
		return nil, fmt.Errorf("el valor no es JSON válido: %w", err)
	}
	return clone, nil
}

func cloneDocument(document Document) (Document, error) {
	clone, err := cloneJSONValue(document)
	if err != nil {
		return nil, err
	}
	result, ok := clone.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("el documento de flujo debe ser un objeto JSON")
	}
	return Document(result), nil
}

func objectField(parent map[string]any, field string) (map[string]any, error) {
	value, exists := parent[field]
	if !exists {
		return nil, fmt.Errorf("el documento requiere %q", field)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%q debe ser un objeto", field)
	}
	return object, nil
}

func objectArrayField(parent map[string]any, field string) ([]map[string]any, error) {
	value, exists := parent[field]
	if !exists {
		return nil, fmt.Errorf("el documento requiere %q", field)
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%q debe ser una lista", field)
	}
	result := make([]map[string]any, 0, len(items))
	for index, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s[%d] debe ser un objeto", field, index)
		}
		result = append(result, object)
	}
	return result, nil
}

func storeObjectArray(parent map[string]any, field string, items []map[string]any) {
	values := make([]any, len(items))
	for index := range items {
		values[index] = items[index]
	}
	parent[field] = values
}
