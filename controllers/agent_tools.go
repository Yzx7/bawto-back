package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Yzx7/sacs-chatbots/channels"
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

// maxAgentQueryResults acota lo que vuelve al modelo. Cada resultado se reenvía en
// todas las iteraciones siguientes del turno, así que un catálogo entero encarece
// el bucle completo, no solo el paso que lo pidió.
const (
	maxAgentQueryResults = 8
	maxAgentImageBytes   = 10 * 1024 * 1024
)

// agentMediaSource identifica el mensaje durable que puede ver un agente.
// El flujo habilita imagen, pero nunca elige IDs, organización, URL ni bytes.
type agentMediaSource struct {
	MessageID int64
	Inbound   channels.InboundMessage
}

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
		case "data_query":
			return con.execAgentDataQuery(ctx, bot, config[name], input)
		default:
			return "", fmt.Errorf("herramienta %q sin ejecutor", name)
		}
	}
	return specs, exec, nil
}

// execAgentDataQuery es la misma lectura que usa el grafo, con una diferencia de
// confianza: aquí los argumentos los redacta el modelo, así que el alcance se
// impone antes de consultar. El objeto viene del bloque; los campos por los que
// puede filtrar también. Un campo que el autor no listó no se rechaza con un
// error técnico: se le dice al modelo por qué no vale, que es lo que le permite
// reintentar bien en vez de repetir la misma llamada.
func (con *Controller) execAgentDataQuery(ctx context.Context, bot *models.BotChannel, config map[string]string, input json.RawMessage) (string, error) {
	objectKeys := splitList(config["objects"])
	if objectKey := strings.TrimSpace(config["object"]); objectKey != "" {
		objectKeys = []string{objectKey}
	}
	if len(objectKeys) == 0 {
		return "", fmt.Errorf("la herramienta no tiene objeto de datos configurado")
	}
	var args struct {
		Query   string `json:"query"`
		Filters []struct {
			Field string `json:"field"`
			Op    string `json:"op"`
			Value string `json:"value"`
		} `json:"filters"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("argumentos inválidos")
	}

	allowed := splitList(config["filterFields"])
	rules := make([]models.DataQueryRule, 0, len(args.Filters))
	for _, filter := range args.Filters {
		field := strings.TrimSpace(filter.Field)
		if !containsString(allowed, field) {
			if len(allowed) == 0 {
				return "Esta herramienta no admite filtros por campo. Vuelve a llamarla usando solo `query`.", nil
			}
			return fmt.Sprintf("No puedes filtrar por %q. Campos permitidos: %s.",
				field, strings.Join(allowed, ", ")), nil
		}
		op := strings.TrimSpace(filter.Op)
		if op != "eq" && op != "contains" && op != "in" {
			return fmt.Sprintf("El operador %q no está permitido. Usa eq, contains o in.", op), nil
		}
		rules = append(rules, models.DataQueryRule{Field: field, Op: op, Value: filter.Value})
	}

	limit := maxAgentQueryResults
	if raw := strings.TrimSpace(config["maxLimit"]); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	var b strings.Builder
	total := 0
	for _, objectKey := range objectKeys {
		result, err := models.QueryDataRecords(ctx, con.Env.Postgres, models.DataQueryInput{
			OrgID: bot.OrgID, ObjectKey: objectKey, Fields: splitList(config["fields"]),
			Where: rules, Text: args.Query, Limit: limit,
		})
		if err != nil {
			return "", err
		}
		if !result.Found {
			continue
		}
		fmt.Fprintf(&b, "%d resultado(s) en %s:\n", result.Count, objectKey)
		for _, record := range result.Records {
			total++
			fmt.Fprintf(&b, "\n%d. %s", total, renderValues(record.Data))
			if total >= limit {
				break
			}
		}
		if total >= limit {
			break
		}
	}
	if total == 0 {
		// Se le dice explícitamente que no hay nada, en vez de devolver vacío: un
		// resultado en blanco invita a rellenar el hueco inventando.
		return fmt.Sprintf("Sin resultados en %s. No hay información registrada sobre eso.", strings.Join(objectKeys, ", ")), nil
	}
	return b.String(), nil
}

// renderValues aplana el registro a `clave: valor` en orden estable. Se entrega
// como texto y no como JSON crudo porque el modelo lo lee mejor y porque así no
// se le enseña la forma interna de la tabla.
func renderValues(values map[string]any) string {
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
//
// tenantID aísla el caché de prefijo del proveedor. Se pasa la organización: es
// la frontera de confianza real de una plataforma multiempresa, y deja el caché
// del system prompt compartido entre los bots y los chats de una misma empresa,
// que es donde está el ahorro. Bajarlo al bot es cambiar el argumento en el
// webhook, sin tocar nada de aquí para abajo.
func (con *Controller) runAgent(ctx context.Context, tenantID string, request engine.AgentRequest,
	agentTools []ai.AgentTool, exec ai.ToolExecutor) (engine.AgentResult, ai.Usage, error) {
	configured := con.Env.TextAgent
	if containsString(request.Accepts, "image") {
		configured = con.Env.VisionAgent
		if configured == nil {
			return engine.AgentResult{}, ai.Usage{}, fmt.Errorf("agente visual no configurado: falta MINIMAX_M3_API_KEY")
		}
	} else if request.AgentRole == "orchestrator" {
		configured = con.Env.OrchestratorAgent
		if configured == nil {
			return engine.AgentResult{}, ai.Usage{}, fmt.Errorf("orquestador no configurado: falta MINIMAX_M3_API_KEY")
		}
	} else if configured == nil {
		return engine.AgentResult{}, ai.Usage{}, fmt.Errorf("agente de texto no configurado")
	}
	agent := configured.ForTenant(tenantID)
	if len(agentTools) == 0 {
		return agent.RunStructuredWithHistoryUsage(ctx, request.Instruction, request.Vars,
			request.Outputs, request.History, request.Silent, request.OutputFields, request.Media)
	}
	return agent.RunStructuredAgenticUsage(ctx, request.Instruction, request.Vars,
		request.Outputs, request.History, request.Silent, request.OutputFields, request.Media, agentTools, exec)
}

// attachCurrentAgentMedia hidrata únicamente la imagen que disparó este turno.
// La configuración del grafo solo puede habilitar el formato; nunca puede
// elegir el messageID, bot, organización, URL ni bytes que se envían al modelo.
func (con *Controller) attachCurrentAgentMedia(ctx context.Context, bot *models.BotChannel, request *engine.AgentRequest, session *agentMediaSource) error {
	if request == nil || !containsString(request.Accepts, "image") || session == nil || session.Inbound.Type != channels.MsgImage {
		return nil
	}
	if session.MessageID <= 0 {
		return fmt.Errorf("el agente visual requiere la imagen del mensaje actual")
	}
	media, err := models.GetMessageMedia(ctx, con.Env.Postgres, session.MessageID)
	if err != nil {
		return err
	}
	if media == nil || media.OrgID != bot.OrgID || media.BotID != bot.ID || media.ProviderID != session.Inbound.MediaID {
		return fmt.Errorf("la imagen del mensaje no existe o no pertenece al bot")
	}
	if len(media.Data) == 0 || len(media.Data) > maxAgentImageBytes {
		return fmt.Errorf("la imagen del agente está vacía o supera %d bytes", maxAgentImageBytes)
	}
	request.Media = &engine.AgentMedia{
		Data: media.Data, MIMEType: media.MimeType, Caption: session.Inbound.Caption,
	}
	return nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
