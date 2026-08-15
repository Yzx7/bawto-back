package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Yzx7/sacs-chatbots/authoring"
	"github.com/Yzx7/sacs-chatbots/engine"
	"github.com/Yzx7/sacs-chatbots/models"
)

// toolHandler tiene la forma de una expresión de método para que el mapa de
// abajo se lea como el índice de las cuatro capacidades del §4.
type toolHandler func(s *session, ctx context.Context, arguments json.RawMessage) (any, error)

var toolHandlers = map[string]toolHandler{
	"flow_get":      (*session).flowGet,
	"flow_spec":     (*session).flowSpec,
	"flow_validate": (*session).flowValidate,
	"flow_put":      (*session).flowPut,
}

// toolDefinition es el contrato que ve el cliente MCP. La descripción la lee el
// modelo antes de elegir, así que dice cuándo usar cada una y qué NO hace.
type toolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func toolDefinitions() []toolDefinition {
	return []toolDefinition{
		{
			Name: "flow_get",
			Description: "Lee. Sin argumentos lista los bots de la organización; con botId lista sus flujos; " +
				"con botId y flowId devuelve el JSON completo del borrador y su draftChecksum, que es el valor " +
				"que flow_put exigirá. Devuelve el documento tal cual está guardado, incluidas las posiciones " +
				"del editor: no lo recortes al editarlo.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "botId": {"type": "string", "description": "bot de la organización del token"},
    "flowId": {"type": "string", "description": "flujo del bot; requiere botId"}
  },
  "additionalProperties": false
}`),
		},
		{
			Name: "flow_spec",
			Description: "Aprende cómo se construye un flujo: tipos de nodo con sus campos y puertos, catálogo " +
				"de herramientas del runtime, playbooks y las reglas duras del motor. Incluye catalogHash: si " +
				"cacheas estas reglas, compáralo antes de volver a fiarte de ellas.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "section": {"type": "string", "enum": ["all", "nodes", "tools", "playbooks", "rules"], "description": "por defecto all"},
    "playbookId": {"type": "string", "description": "devuelve el bundle completo de ese playbook"},
    "playbookVersion": {"type": "string", "description": "versión exacta; vacío toma la más alta"}
  },
  "additionalProperties": false
}`),
		},
		{
			Name: "flow_validate",
			Description: "Valida un JSON candidato SIN escribir nada y sin coste. Es la operación del ciclo de " +
				"prueba y error: llámala tantas veces como quieras. Devuelve errores del motor, avisos de " +
				"calidad y, si pasas botId, si las tablas, campos y plantillas que referencias existen de verdad " +
				"en la organización. También devuelve el checksum canónico del candidato.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "flow": {"description": "el documento completo del flujo, como objeto o como texto JSON"},
    "botId": {"type": "string", "description": "opcional; activa la comprobación contra los recursos reales"},
    "triggerType": {"type": "string", "description": "opcional; comprueba que el trigger sea el esperado"}
  },
  "required": ["flow"],
  "additionalProperties": false
}`),
		},
		{
			Name: "flow_put",
			Description: "Escribe el documento completo como BORRADOR. NO publica: el bot sigue atendiendo la " +
				"versión publicada hasta que una persona publique desde el panel. expectedChecksum es " +
				"obligatorio y debe ser el draftChecksum del flow_get más reciente; si alguien tocó el flujo " +
				"mientras trabajabas, falla y hay que volver a leer y fusionar, nunca reenviar la copia vieja.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "botId": {"type": "string"},
    "flowId": {"type": "string"},
    "flow": {"description": "el documento completo del flujo, como objeto o como texto JSON"},
    "expectedChecksum": {"type": "string", "description": "draftChecksum devuelto por el último flow_get"}
  },
  "required": ["botId", "flowId", "flow", "expectedChecksum"],
  "additionalProperties": false
}`),
		},
	}
}

// --- flow_get ---------------------------------------------------------------

type botView struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Channel string  `json:"channel"`
	Phone   *string `json:"phone,omitempty"`
}

type flowView struct {
	ID                 string    `json:"id"`
	Key                string    `json:"key"`
	Name               string    `json:"name"`
	TriggerType        string    `json:"triggerType"`
	Status             string    `json:"status"`
	Priority           int       `json:"priority"`
	IsFallback         bool      `json:"isFallback"`
	PublishedVersion   *int      `json:"publishedVersion,omitempty"`
	UnpublishedChanges bool      `json:"unpublishedChanges"`
	DraftChecksum      string    `json:"draftChecksum"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

func (s *session) flowGet(ctx context.Context, arguments json.RawMessage) (any, error) {
	var input struct {
		BotID  string `json:"botId"`
		FlowID string `json:"flowId"`
	}
	if err := decodeArguments(arguments, &input); err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.BotID) == "" {
		if strings.TrimSpace(input.FlowID) != "" {
			return nil, faultf("missing_bot", "flowId necesita su botId: un flujo se identifica dentro de un bot")
		}
		bots, err := s.store.ListBots(ctx, s.key.OrgID)
		if err != nil {
			return nil, err
		}
		views := make([]botView, 0, len(bots))
		for _, bot := range bots {
			// El listado se filtra aquí y no se deja fallar después: enseñar un
			// bot que luego rechaza toda lectura le dice al modelo —y a quien
			// lea la transcripción— que existe. Un listado es una respuesta tan
			// informativa como una lectura.
			visible, err := s.botHasVisibleFlows(ctx, bot.ID)
			if err != nil {
				return nil, err
			}
			if !visible {
				continue
			}
			views = append(views, botView{ID: bot.ID, Name: bot.Name, Channel: bot.Channel, Phone: bot.Phone})
		}
		return map[string]any{"organizationId": s.key.OrgID, "bots": views}, nil
	}

	if strings.TrimSpace(input.FlowID) == "" {
		flows, err := s.visibleFlows(ctx, input.BotID)
		if err != nil {
			return nil, err
		}
		views := make([]flowView, 0, len(flows))
		for i := range flows {
			view, err := toFlowView(&flows[i])
			if err != nil {
				return nil, err
			}
			views = append(views, view)
		}
		return map[string]any{"botId": input.BotID, "flows": views}, nil
	}

	flow, err := s.requireFlow(ctx, input.BotID, input.FlowID)
	if err != nil {
		return nil, err
	}
	view, err := toFlowView(flow)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"botId": input.BotID,
		"flow":  view,
		// El borrador va crudo, sin normalizar por engine.Flow: el documento
		// lleva campos que el motor no conoce —`pos` de cada nodo— y pasarlo por
		// la struct los borraría, así que devolverlo "limpio" destruiría el
		// layout en cuanto el modelo reenviara lo que leyó (CLAUDE.md §3).
		"draft": json.RawMessage(flow.Draft),
	}, nil
}

func toFlowView(flow *models.Flow) (flowView, error) {
	_, checksum, err := engine.CanonicalChecksum(flow.Draft)
	if err != nil {
		return flowView{}, fmt.Errorf("checksum del borrador de %s: %w", flow.ID, err)
	}
	return flowView{
		ID: flow.ID, Key: flow.Key, Name: flow.Name, TriggerType: flow.TriggerType,
		Status: flow.Status, Priority: flow.Priority, IsFallback: flow.IsFallback,
		PublishedVersion: flow.PublishedVersion, UnpublishedChanges: flow.UnpublishedChanges,
		DraftChecksum: checksum, UpdatedAt: flow.UpdatedAt,
	}, nil
}

// --- flow_spec --------------------------------------------------------------

// authoringRules son los invariantes que un editor incremental nunca tiene que
// explicar porque los impone paso a paso. Quien entrega el flujo entero sí:
// sin esto el modelo produce grafos que validan y significan otra cosa.
var authoringRules = []string{
	"El borrador es mutable y la versión publicada es inmutable: escribir un borrador no cambia lo que el bot atiende.",
	"engine.Validate es la única autoridad sobre qué grafo se puede publicar; flow_validate ejecuta esa misma función.",
	"Las salidas declaradas de un agente son obligatorias y son su decisión de control: cada rama necesita su arista.",
	"Un orquestador (agentRole=orchestrator) no puede tener herramientas; los especialistas sí.",
	"Un flujo schedule no admite agentes, herramientas ni esperas: fuera de la ventana de 24 h solo se envía una plantilla aprobada.",
	"En data_query y data_mutate solo el value interpola variables; objeto, campos y operadores no pueden llevar '{'.",
	"Cero coincidencias en data_query es la rama ok con found=false; la rama error queda para configuración inválida o consulta rota.",
	"Toda ruta error de una herramienta debe llegar a un mensaje, una espera, un fin o una derivación: no se termina en silencio.",
	"Un ciclo conversacional pasa siempre por un wait; un ciclo automático sin pausa es un grafo inválido.",
	"Conserva el campo pos de cada nodo y cualquier clave que no conozcas: el documento sobrevive a backends que no las entienden.",
}

func (s *session) flowSpec(_ context.Context, arguments json.RawMessage) (any, error) {
	var input struct {
		Section         string `json:"section"`
		PlaybookID      string `json:"playbookId"`
		PlaybookVersion string `json:"playbookVersion"`
	}
	if err := decodeArguments(arguments, &input); err != nil {
		return nil, err
	}
	// catalogHash va en todas las respuestas, también en las parciales: quien
	// cacheó una sección tiene que poder detectar que caducó sin pedir el todo.
	//
	// access viaja junto a las reglas porque el alcance ES una regla de cómo se
	// puede construir aquí. Decirlo de entrada no filtra nada —el token ya sabe
	// lo suyo— y evita que el modelo descubra sus límites a base de errores.
	payload := map[string]any{
		"catalogHash": authoring.CatalogHash(),
		"access":      s.accessSummary(),
	}

	if id := strings.TrimSpace(input.PlaybookID); id != "" {
		bundle, found := authoring.GetPlaybook(id, strings.TrimSpace(input.PlaybookVersion))
		if !found {
			return nil, faultf("unknown_playbook", "no existe el playbook %q en la versión pedida", id)
		}
		payload["playbook"] = bundle
		return payload, nil
	}

	section := strings.TrimSpace(input.Section)
	if section == "" {
		section = "all"
	}
	switch section {
	case "all":
		payload["nodeKinds"] = authoring.NodeCatalog()
		payload["runtimeTools"] = authoring.RuntimeToolCatalog()
		payload["playbooks"] = authoring.ListPlaybooks()
		payload["rules"] = authoringRules
	case "nodes":
		payload["nodeKinds"] = authoring.NodeCatalog()
	case "tools":
		payload["runtimeTools"] = authoring.RuntimeToolCatalog()
	case "playbooks":
		payload["playbooks"] = authoring.ListPlaybooks()
	case "rules":
		payload["rules"] = authoringRules
	default:
		return nil, faultf("unknown_section", "section %q no existe; usa all, nodes, tools, playbooks o rules", section)
	}
	return payload, nil
}

// --- flow_validate ----------------------------------------------------------

// validationReport separa deliberadamente dos preguntas que un modelo confunde
// si se le da un único `ok`: si el motor aceptaría publicar el grafo, y si
// además no queda ninguna observación de autoría. El borrador vivo de
// Sistemuino pasa engine.Validate y aun así acumula errores de autoría —dos
// bloques que escriben la misma variable, un campo obligatorio sin valor—; con
// un solo booleano, una IA se pondría a "arreglar" un flujo que el panel
// publica sin protestar, y acabaría reescribiendo trabajo ajeno.
type validationReport struct {
	OK               bool                   `json:"ok"`
	EngineValid      bool                   `json:"engineValid"`
	Checksum         string                 `json:"checksum"`
	Errors           int                    `json:"errors"`
	Warnings         int                    `json:"warnings"`
	ResourcesChecked bool                   `json:"resourcesChecked"`
	Diagnostics      []authoring.Diagnostic `json:"diagnostics"`
}

func (s *session) flowValidate(ctx context.Context, arguments json.RawMessage) (any, error) {
	var input struct {
		Flow        json.RawMessage `json:"flow"`
		BotID       string          `json:"botId"`
		TriggerType string          `json:"triggerType"`
	}
	if err := decodeArguments(arguments, &input); err != nil {
		return nil, err
	}
	candidate, err := decodeFlowDocument(input.Flow)
	if err != nil {
		return nil, err
	}
	var resources authoring.AuthoringResourceSnapshot
	checked := false
	if botID := strings.TrimSpace(input.BotID); botID != "" {
		// Aquí también se exige alcance: el snapshot de recursos expone las
		// tablas, campos y plantillas de la organización. Validar es gratis,
		// pero eso no lo convierte en una operación sin lectura de datos.
		if err := s.requireBotInScope(ctx, botID); err != nil {
			return nil, err
		}
		resources, err = s.store.Resources(ctx, s.key.OrgID, botID)
		if err != nil {
			return nil, err
		}
		checked = true
	}
	report := buildReport(candidate, resources, checked)
	if expected := strings.TrimSpace(input.TriggerType); expected != "" {
		if actual := documentTriggerType(candidate); actual != expected {
			report.Diagnostics = append(report.Diagnostics, authoring.Diagnostic{
				Severity: authoring.SeverityError, Source: authoring.SourceEngine,
				Code: "trigger_type_mismatch", Path: "trigger.type",
				Message: fmt.Sprintf("el trigger es %q y se esperaba %q", actual, expected),
			})
			report.Errors++
			report.OK = false
		}
	}
	return report, nil
}

// buildReport no reimplementa nada: ValidateForAuthoring ya encadena
// engine.Validate, los bindings del tenant y el lint de calidad. Cuando no hay
// snapshot de recursos el paso de bindings simplemente no encuentra nada que
// contrastar, y por eso ResourcesChecked viaja en la respuesta: un modelo no
// debe leer «sin errores» como «la tabla existe».
func buildReport(candidate []byte, resources authoring.AuthoringResourceSnapshot, checked bool) validationReport {
	diagnostics := authoring.ValidateForAuthoring(candidate, resources).Diagnostics
	report := validationReport{OK: true, EngineValid: true, ResourcesChecked: checked, Diagnostics: diagnostics}
	for _, diagnostic := range diagnostics {
		// engine_invalid es el código con el que ValidateForAuthoring envuelve el
		// veredicto de engine.Validate; leerlo de ahí evita analizar el documento
		// dos veces en la operación que más se va a llamar.
		if diagnostic.Code == "engine_invalid" {
			report.EngineValid = false
		}
		if diagnostic.Severity == authoring.SeverityError {
			report.Errors++
			report.OK = false
			continue
		}
		report.Warnings++
	}
	if _, checksum, err := engine.CanonicalChecksum(candidate); err == nil {
		report.Checksum = checksum
	}
	return report
}

// --- flow_put ---------------------------------------------------------------

func (s *session) flowPut(ctx context.Context, arguments json.RawMessage) (any, error) {
	var input struct {
		BotID            string          `json:"botId"`
		FlowID           string          `json:"flowId"`
		Flow             json.RawMessage `json:"flow"`
		ExpectedChecksum string          `json:"expectedChecksum"`
	}
	if err := decodeArguments(arguments, &input); err != nil {
		return nil, err
	}
	// El checksum no tiene valor por defecto ni modo «forzar». El 2026-08-14 una
	// escritura desde una exportación vieja revirtió 19 posiciones que el dueño
	// había movido en el panel; con varias IAs escribiendo a la vez, un put sin
	// checksum reproduce ese accidente cada día.
	expected := strings.TrimSpace(input.ExpectedChecksum)
	if expected == "" {
		return nil, faultf("missing_checksum",
			"expectedChecksum es obligatorio: léelo con flow_get inmediatamente antes de escribir")
	}
	candidate, err := decodeFlowDocument(input.Flow)
	if err != nil {
		return nil, err
	}
	flow, err := s.requireFlow(ctx, input.BotID, input.FlowID)
	if err != nil {
		return nil, err
	}
	// El tipo de disparo se comprueba antes que la estructura porque decide qué
	// reglas se aplican: un documento marcado como schedule falla como grafo
	// programado —«solo puede enviar plantillas»— y ese mensaje despistaría a
	// quien en realidad solo se equivocó de tipo.
	if actual := documentTriggerType(candidate); actual != flow.TriggerType {
		return nil, faultf("trigger_type_mismatch",
			"este flujo es de tipo %q y el documento trae %q; el tipo de disparo no se cambia editando el grafo",
			flow.TriggerType, actual)
	}
	// Un borrador puede quedar a medias —el panel también lo permite— porque
	// exigir un grafo publicable en cada guardado rompe el ciclo de prueba y
	// error. Lo que no se admite es un documento cuyos nodos o aristas no tengan
	// forma: eso no es un paso intermedio, es basura persistida.
	if err := authoring.ValidateIntermediateCandidate(candidate); err != nil {
		return nil, faultf("invalid_document", "el documento no tiene forma de flujo: %v", err)
	}

	snapshot, err := s.store.UpdateDraft(ctx, input.BotID, input.FlowID, candidate, expected, s.actor())
	if err != nil {
		var conflict *models.DraftConflictError
		if errors.As(err, &conflict) {
			return nil, &toolFault{
				Code: "draft_conflict",
				Message: "el borrador cambió en otra sesión: no reenvíes esta copia. Vuelve a leerlo con " +
					"flow_get, fusiona tu cambio sobre el estado vivo —incluidas posiciones y conexiones " +
					"que haya movido una persona— y reintenta con el checksum nuevo.",
				Data: map[string]any{
					"expectedChecksum": conflict.ExpectedChecksum,
					"currentChecksum":  conflict.CurrentChecksum,
					"currentUpdatedAt": conflict.CurrentUpdatedAt,
				},
			}
		}
		return nil, err
	}
	if snapshot == nil {
		return nil, faultf("flow_not_found", "el flujo dejó de existir antes de escribir")
	}

	// changed compara contra lo que había, no contra expectedChecksum. Cuando el
	// candidato es idéntico al borrador vivo, models devuelve el snapshot actual
	// sin escribir —y lo hace **antes** de mirar expectedChecksum, para que
	// reintentar una escritura cuyo response se perdió sea idempotente—. Medirlo
	// contra el checksum que mandó el cliente diría «cambió» en ese reintento, y
	// el modelo se creería autor de una edición que no existió.
	_, previous, err := engine.CanonicalChecksum(flow.Draft)
	if err != nil {
		return nil, err
	}
	resources, resourceErr := s.store.Resources(ctx, s.key.OrgID, input.BotID)
	report := buildReport(snapshot.Draft, resources, resourceErr == nil)
	note := "Guardado como borrador. El bot sigue ejecutando la versión publicada; " +
		"publicar es una acción humana desde el panel."
	if snapshot.Checksum == previous {
		note = "El borrador ya era idéntico a este documento: no se escribió nada. " +
			"El bot sigue ejecutando la versión publicada."
	}
	return map[string]any{
		"saved":         true,
		"published":     false,
		"draftChecksum": snapshot.Checksum,
		"changed":       snapshot.Checksum != previous,
		"updatedAt":     snapshot.UpdatedAt,
		"validation":    report,
		"note":          note,
	}, nil
}

// --- comunes ----------------------------------------------------------------

// accessSummary describe lo que este token alcanza. Se devuelve tal cual sale
// del token firmado, sin consultar la base: es una descripción de la credencial,
// no un inventario de la organización.
func (s *session) accessSummary() map[string]any {
	summary := map[string]any{"organizationId": s.key.OrgID, "scoped": s.key.Scoped()}
	if s.key.Scoped() {
		summary["flowIds"] = s.key.FlowIDs
		summary["note"] = "Este token solo alcanza los flujos listados. El resto de la organización " +
			"responde como si no existiera."
		return summary
	}
	summary["note"] = "Este token alcanza todos los flujos de su organización."
	return summary
}

// requireBot es el aislamiento por organización. La organización sale del token
// firmado y jamás de los argumentos, así que un botId de otro tenant —pegado en
// un prompt o inventado— no llega a tocar la base. El mensaje no distingue
// «no existe» de «es de otra organización»: responder distinto convertiría esta
// herramienta en un detector de bots ajenos.
func (s *session) requireBot(ctx context.Context, botID string) (*models.Bot, error) {
	botID = strings.TrimSpace(botID)
	if botID == "" {
		return nil, faultf("missing_bot", "botId es obligatorio")
	}
	bot, err := s.store.GetBot(ctx, botID)
	if err != nil {
		return nil, err
	}
	if bot == nil || bot.OrgID != s.key.OrgID {
		return nil, faultf("bot_not_found", "no hay ningún bot %s en esta organización", botID)
	}
	return bot, nil
}

func (s *session) requireFlow(ctx context.Context, botID, flowID string) (*models.Flow, error) {
	if _, err := s.requireBot(ctx, botID); err != nil {
		return nil, err
	}
	flowID = strings.TrimSpace(flowID)
	if flowID == "" {
		return nil, faultf("missing_flow", "flowId es obligatorio")
	}
	flow, err := s.store.GetFlow(ctx, botID, flowID)
	if err != nil {
		return nil, err
	}
	// Fuera de alcance y inexistente comparten respuesta, por lo mismo que la
	// comparten «no existe» y «es de otra organización»: si un token acotado
	// pudiera separar los dos casos, sería un detector de los flujos que no le
	// dieron. Un flujo alcanzable implica que su bot lo es, así que aquí no hace
	// falta la consulta derivada.
	if flow == nil || !s.key.AllowsFlow(flow.ID) {
		return nil, faultf("flow_not_found", "el bot %s no tiene el flujo %s", botID, flowID)
	}
	return flow, nil
}

// requireBotInScope se usa cuando la petición nombra un bot pero ningún flujo.
// Un token completo no paga el listado: para él todo bot de su organización
// está en alcance.
func (s *session) requireBotInScope(ctx context.Context, botID string) error {
	if _, err := s.requireBot(ctx, botID); err != nil {
		return err
	}
	if !s.key.Scoped() {
		return nil
	}
	visible, err := s.botHasVisibleFlows(ctx, botID)
	if err != nil {
		return err
	}
	if !visible {
		return faultf("bot_not_found", "no hay ningún bot %s en esta organización", botID)
	}
	return nil
}

func (s *session) botHasVisibleFlows(ctx context.Context, botID string) (bool, error) {
	if !s.key.Scoped() {
		return true, nil
	}
	flows, err := s.store.ListFlows(ctx, botID)
	if err != nil {
		return false, err
	}
	for i := range flows {
		if s.key.AllowsFlow(flows[i].ID) {
			return true, nil
		}
	}
	return false, nil
}

// visibleFlows devuelve los flujos del bot que el token puede ver. Filtra antes
// de responder en vez de listar todo y rechazar después, que es lo que pidió el
// dueño: un token acotado no debe ni enterarse de los demás.
func (s *session) visibleFlows(ctx context.Context, botID string) ([]models.Flow, error) {
	if _, err := s.requireBot(ctx, botID); err != nil {
		return nil, err
	}
	flows, err := s.store.ListFlows(ctx, botID)
	if err != nil {
		return nil, err
	}
	if !s.key.Scoped() {
		return flows, nil
	}
	visible := make([]models.Flow, 0, len(flows))
	for i := range flows {
		if s.key.AllowsFlow(flows[i].ID) {
			visible = append(visible, flows[i])
		}
	}
	// Ni un bot vacío se distingue de un bot inexistente: responder «existe pero
	// no ves nada» vuelve a delatar lo que el alcance oculta.
	if len(visible) == 0 {
		return nil, faultf("bot_not_found", "no hay ningún bot %s en esta organización", botID)
	}
	return visible, nil
}

func decodeArguments(arguments json.RawMessage, target any) error {
	if len(bytes.TrimSpace(arguments)) == 0 || string(bytes.TrimSpace(arguments)) == "null" {
		return nil
	}
	if err := json.Unmarshal(arguments, target); err != nil {
		return faultf("invalid_arguments", "argumentos inválidos: %v", err)
	}
	return nil
}

// decodeFlowDocument acepta el flujo como objeto o como texto JSON. No es una
// concesión estética: muchos modelos entregan documentos grandes serializados
// dentro de una cadena, y rechazarlos costaría un reintento entero por cada
// vuelta del ciclo.
func decodeFlowDocument(raw json.RawMessage) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, faultf("missing_flow_document", "flow es obligatorio")
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, faultf("invalid_flow_document", "flow venía como texto pero no se pudo leer: %v", err)
		}
		trimmed = bytes.TrimSpace([]byte(text))
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, faultf("invalid_flow_document", "flow debe ser el documento completo del flujo (un objeto JSON)")
	}
	if _, _, err := engine.CanonicalChecksum(trimmed); err != nil {
		return nil, faultf("invalid_flow_document", "%v", err)
	}
	return trimmed, nil
}

// documentTriggerType lee el trigger del documento genérico en vez de la struct
// tipada para no depender de que el resto del grafo decodifique.
func documentTriggerType(raw []byte) string {
	var document struct {
		Trigger struct {
			Type string `json:"type"`
		} `json:"trigger"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return ""
	}
	return document.Trigger.Type
}
