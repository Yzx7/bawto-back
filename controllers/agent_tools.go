package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Yzx7/sacs-chatbots/engine"
	"github.com/Yzx7/sacs-chatbots/engine/ai"
	"github.com/Yzx7/sacs-chatbots/engine/tools"
	"github.com/Yzx7/sacs-chatbots/models"
)

// Puente entre el registro de herramientas y el modelo.
//
// El registro (`engine/tools`) dice **qué** existe y no puede tocar la base de
// datos; aquí viven los ejecutores, que sí. La configuración que fijó el autor
// del flujo se aplica en este lado a propósito: el modelo redacta los argumentos
// de su esquema, pero no puede cambiar el alcance —en qué tabla busca— porque ese
// dato nunca llega a él.

// maxSearchResults acota lo que vuelve al modelo. Cada resultado se reenvía en
// todas las iteraciones siguientes del turno, así que un catálogo entero encarece
// el bucle completo, no solo el paso que lo pidió.
const maxSearchResults = 8

// agentTooling traduce las herramientas declaradas en el nodo a lo que el
// adaptador de IA necesita: las fichas que ve el modelo y un ejecutor que
// despacha por nombre.
func (con *Controller) agentTooling(ctx context.Context, bot *models.BotChannel, nodeTools []engine.NodeTool) ([]ai.AgentTool, ai.ToolExecutor, error) {
	if len(nodeTools) == 0 {
		return nil, nil, nil
	}
	specs := make([]ai.AgentTool, 0, len(nodeTools))
	config := make(map[string]map[string]string, len(nodeTools))
	for _, nodeTool := range nodeTools {
		spec := tools.Get(nodeTool.Ref)
		if spec == nil || !spec.ForAgent {
			return nil, nil, fmt.Errorf("herramienta %q no disponible para agentes", nodeTool.Ref)
		}
		specs = append(specs, ai.AgentTool{
			Name:        spec.Name,
			Description: spec.Description,
			InputSchema: spec.InputSchema,
		})
		config[nodeTool.Ref] = nodeTool.Config
	}

	exec := func(ctx context.Context, name string, input json.RawMessage) (string, error) {
		switch name {
		case "search_data":
			return con.execSearchData(ctx, bot, config[name], input)
		default:
			return "", fmt.Errorf("herramienta %q sin ejecutor", name)
		}
	}
	return specs, exec, nil
}

// execSearchData busca en el objeto de datos que fijó el autor del nodo.
func (con *Controller) execSearchData(ctx context.Context, bot *models.BotChannel, config map[string]string, input json.RawMessage) (string, error) {
	objectKey := strings.TrimSpace(config["object"])
	if objectKey == "" {
		return "", fmt.Errorf("la herramienta no tiene objeto de datos configurado")
	}
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("argumentos inválidos")
	}

	records, err := models.SearchDataRecordsByOrg(ctx, con.Env.Postgres, bot.OrgID, objectKey,
		args.Query, maxSearchResults)
	if err != nil {
		return "", err
	}
	if len(records) == 0 {
		// Se le dice explícitamente que no hay nada, en vez de devolver vacío: un
		// resultado en blanco invita a rellenar el hueco inventando.
		return fmt.Sprintf("Sin resultados para %q en %s. No hay información registrada sobre eso.",
			args.Query, objectKey), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d resultado(s) en %s:\n", len(records), objectKey)
	for i, record := range records {
		fmt.Fprintf(&b, "\n%d. %s", i+1, renderRecord(record.Data))
	}
	return b.String(), nil
}

// renderRecord aplana el JSON del registro a `clave: valor` en orden estable. Se
// entrega como texto y no como JSON crudo porque el modelo lo lee mejor y porque
// así no se le enseña la forma interna de la tabla.
func renderRecord(raw json.RawMessage) string {
	var values map[string]any
	if json.Unmarshal(raw, &values) != nil {
		return string(raw)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := strings.TrimSpace(fmt.Sprint(values[key]))
		if value == "" || value == "<nil>" {
			continue
		}
		parts = append(parts, key+": "+value)
	}
	return strings.Join(parts, " · ")
}

// runAgent elige el camino según lo que el nodo declare. Sin herramientas se
// mantiene la petición única de siempre: montar un bucle para un nodo que no
// puede llamar nada solo añadiría latencia y coste.
func (con *Controller) runAgent(ctx context.Context, request engine.AgentRequest,
	agentTools []ai.AgentTool, exec ai.ToolExecutor) (string, string, ai.Usage, error) {
	if len(agentTools) == 0 {
		return con.Env.Agent.RunWithHistoryUsage(ctx, request.Instruction, request.Vars,
			request.Outputs, request.History, request.Silent)
	}
	return con.Env.Agent.RunAgenticUsage(ctx, request.Instruction, request.Vars,
		request.Outputs, request.History, request.Silent, agentTools, exec)
}
