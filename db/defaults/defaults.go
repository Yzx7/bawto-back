// Package defaults guarda lo que una organización y un bot traen dentro al
// nacer: las tablas de la organización y el grafo del primer flujo del bot.
//
// Va embebido y no leído del disco porque la semilla corre en producción, dentro
// de la transacción que crea la organización: `db/flows/*.json` solo lo abren las
// pruebas con os.ReadFile y nada de eso viaja en el binario. Mismo patrón que
// `db/migrate.go` con las migraciones.
//
// El paquete tiene dos autores y dos ficheros: aquí viven las tablas; el grafo
// inicial y su injerto comercial están en `injerto.go`.
package defaults

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed objetos.json
var objetosJSON []byte

// SeedField es un campo de una tabla sembrada. Los nombres de los campos
// coinciden con los que acepta el panel en `/orgs/:id/data-objects`
// (`ValidateDataFieldDefinition`) para que la semilla produzca exactamente lo
// mismo que habría creado una persona a mano.
type SeedField struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

// SeedObject es una tabla sembrada con sus campos, en el orden en que el panel
// debe mostrarlos.
type SeedObject struct {
	Key        string      `json:"key"`
	Name       string      `json:"name"`
	PluralName string      `json:"pluralName"`
	Fields     []SeedField `json:"fields"`
}

// Objetos devuelve las tablas que recibe una organización recién creada.
//
// Decodifica en cada llamada a propósito: devolver un slice compartido dejaría
// que un llamador mutara la semilla del proceso entero.
func Objetos() ([]SeedObject, error) {
	var objects []SeedObject
	if err := json.Unmarshal(objetosJSON, &objects); err != nil {
		return nil, fmt.Errorf("objetos.json inválido: %w", err)
	}
	return objects, nil
}
