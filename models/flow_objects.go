package models

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Yzx7/sacs-chatbots/engine"
)

// Aviso al publicar cuando un `object` del grafo no existe en la organización
// (Fase 4 de PLAN-SEMILLA-DE-ORGANIZACION-Y-BOT.md).
//
// La tabla se nombra por clave y se resuelve **en ejecución**, así que un grafo
// que lee una tabla borrada o renombrada publica limpio y falla delante de un
// cliente: `data_query` sale por su rama `error` cuando ya es tarde. La semilla
// garantiza que las claves existan el día que la organización nace; esto cubre
// lo que pase después.
//
// Avisa, **no bloquea**: la tabla es del dueño y puede querer otra, o estar
// creándola en otra pestaña. Convertirlo en error impediría publicar un flujo
// que quizá sea correcto mañana, y este proyecto ya sabe qué cuesta un falso
// positivo que parece un conflicto de concurrencia.

// flowObjectQueryer es lo que hace falta para comprobar: consultar. Lo cumplen el
// pool y la transacción de publicación, que es donde se llama para leer las
// claves dentro del mismo bloqueo que valida el resto.
type flowObjectQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// missingFlowObjectWarningsForBot resuelve la organización del bot y comprueba
// contra ella. Va por el bot y no por un orgID recibido porque quien publica
// habla siempre de un flujo de un bot, y así no hay forma de contrastar el grafo
// contra las tablas de otra organización.
func missingFlowObjectWarningsForBot(
	ctx context.Context,
	q flowObjectQueryer,
	botID string,
	definition json.RawMessage,
) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT org_id::text FROM bots WHERE id = $1::uuid`, botID)
	if err != nil {
		return nil, err
	}
	orgID, err := pgx.CollectExactlyOneRow(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	return missingFlowObjectWarnings(ctx, q, orgID, definition)
}

// missingFlowObjectWarnings devuelve un aviso por cada tabla que el grafo nombra
// y la organización no tiene. Devuelve nil cuando están todas.
func missingFlowObjectWarnings(
	ctx context.Context,
	q flowObjectQueryer,
	orgID string,
	definition json.RawMessage,
) ([]string, error) {
	var flow engine.Flow
	if err := json.Unmarshal(definition, &flow); err != nil {
		return nil, err
	}
	referenced := flowObjectKeys(&flow)
	if len(referenced) == 0 {
		return nil, nil
	}

	rows, err := q.Query(ctx, `SELECT key FROM data_objects WHERE org_id = $1::uuid`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	existing := map[string]struct{}{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		existing[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var warnings []string
	for _, key := range referenced {
		if _, ok := existing[key]; ok {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"la tabla %q no existe en esta organización: los bloques que la usan saldrán por su rama de error", key))
	}
	return warnings, nil
}

// flowObjectKeys reúne las tablas que el grafo nombra, ordenadas y sin repetir.
//
// Mira los dos sitios donde puede aparecer una: el bloque del grafo (`args`) y la
// herramienta que se le ofrece a un agente (`config`). Se ignora cualquier clave
// con `{`: el validador prohíbe interpolar el objeto justo para que el texto del
// cliente no elija qué tabla se lee, pero un grafo viejo podría traerla y no es
// aquí donde se rechaza.
func flowObjectKeys(flow *engine.Flow) []string {
	keys := map[string]struct{}{}
	add := func(key string) {
		key = strings.TrimSpace(key)
		if key == "" || strings.Contains(key, "{") {
			return
		}
		keys[key] = struct{}{}
	}
	for _, node := range flow.Nodes {
		if toolUsesDataObject(node.ToolRef) {
			add(node.Args["object"])
		}
		for _, tool := range node.Tools {
			if toolUsesDataObject(tool.Ref) {
				add(tool.Config["object"])
			}
		}
	}
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// toolUsesDataObject dice si esa herramienta nombra una tabla de la organización.
// Son las dos generales de datos: leer y guardar.
func toolUsesDataObject(ref string) bool {
	return ref == "data_query" || ref == "data_mutate"
}
