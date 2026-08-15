package engine

import (
	"strings"
	"testing"
)

func validAgentFlow() *Flow {
	return &Flow{
		ID:      "flow",
		Name:    "Flujo",
		Trigger: Trigger{Type: "message", Match: "any"},
		Nodes: []Node{
			{ID: "agent", Kind: "agent", Instruction: "Clasifica", Outputs: []string{"ok", "retry"}},
			{ID: "ok", Kind: "action", Action: "end"},
			{ID: "retry", Kind: "wait", Expect: "text"},
		},
		Edges: []Edge{
			{ID: "trigger-agent", Source: "trigger", Target: "agent"},
			{ID: "agent-ok", Source: "agent", SourceHandle: "ok", Target: "ok"},
			{ID: "agent-retry", Source: "agent", SourceHandle: "retry", Target: "retry"},
			{ID: "retry-agent", Source: "retry", Target: "agent", Role: "loopback"},
		},
	}
}

func TestValidateAgentBranches(t *testing.T) {
	if err := Validate(validAgentFlow()); err != nil {
		t.Fatalf("flujo base inválido: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Flow)
		want   string
	}{
		{
			name: "empty branch",
			mutate: func(flow *Flow) {
				flow.Nodes[0].Outputs[0] = ""
			},
			want: "rama \"\" inválida",
		},
		{
			name: "branch with spaces",
			mutate: func(flow *Flow) {
				flow.Nodes[0].Outputs[0] = "con versar"
			},
			want: "rama \"con versar\" inválida",
		},
		{
			name: "duplicate branch",
			mutate: func(flow *Flow) {
				flow.Nodes[0].Outputs = []string{"ok", "OK"}
			},
			want: "rama duplicada",
		},
		{
			name: "undeclared edge",
			mutate: func(flow *Flow) {
				flow.Edges = append(flow.Edges, Edge{
					ID: "agent-other", Source: "agent", SourceHandle: "other", Target: "ok",
				})
			},
			want: "rama no declarada",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flow := validAgentFlow()
			tt.mutate(flow)
			err := Validate(flow)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v; esperaba %q", err, tt.want)
			}
		})
	}
}

func TestValidateAgentStructuredOutput(t *testing.T) {
	valid := validAgentFlow()
	valid.Nodes[0].SaveAs = "comprobante"
	valid.Nodes[0].OutputFields = []AgentOutputField{
		{Key: "provider", Type: "string", Description: "Yape, Plin o banco"},
		{Key: "amount", Type: "number"},
		{Key: "occurredAt", Type: "datetime"},
	}
	if err := Validate(valid); err != nil {
		t.Fatalf("salida enriquecida válida rechazada: %v", err)
	}

	tests := []struct {
		name  string
		field AgentOutputField
		want  string
	}{
		{name: "reserved", field: AgentOutputField{Key: "branch", Type: "string"}, want: "reservada"},
		{name: "duplicate", field: AgentOutputField{Key: "Amount", Type: "number"}, want: "duplicado"},
		{name: "type", field: AgentOutputField{Key: "confidence", Type: "decimal"}, want: "tipo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flow := validAgentFlow()
			flow.Nodes[0].SaveAs = "resultado"
			flow.Nodes[0].OutputFields = []AgentOutputField{{Key: "amount", Type: "number"}, tt.field}
			err := Validate(flow)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v; se esperaba %q", err, tt.want)
			}
		})
	}

	missingNamespace := validAgentFlow()
	missingNamespace.Nodes[0].OutputFields = []AgentOutputField{{Key: "amount", Type: "number"}}
	if err := Validate(missingNamespace); err == nil || !strings.Contains(err.Error(), "saveAs") {
		t.Fatalf("se esperaba exigir saveAs, err=%v", err)
	}
}

func TestValidateVisionAgentAcceptsDirectImage(t *testing.T) {
	flow := validAgentFlow()
	flow.Trigger.Accepts = []string{"image"}
	flow.Trigger.RouteBy = "content_type"
	flow.Edges[0].SourceHandle = "image"
	if err := Validate(flow); err == nil || !strings.Contains(err.Error(), "no declara entrada image") {
		t.Fatalf("un agente de texto debía rechazar la imagen directa, err=%v", err)
	}

	flow.Nodes[0].Accepts = []string{"text", "interactive", "image"}
	if err := Validate(flow); err != nil {
		t.Fatalf("agente visual rechazado: %v", err)
	}
}

func TestValidateTypedTrigger(t *testing.T) {
	flow := &Flow{
		ID: "typed", Name: "Tipado",
		Trigger: Trigger{Type: "message", Match: "any", Accepts: []string{"text", "image"}, RouteBy: "content_type"},
		Nodes:   []Node{{ID: "text", Kind: "action", Action: "end"}, {ID: "image", Kind: "action", Action: "end"}},
		Edges: []Edge{
			{ID: "t", Source: "trigger", SourceHandle: "text", Target: "text"},
			{ID: "i", Source: "trigger", SourceHandle: "image", Target: "image"},
		},
	}
	if err := Validate(flow); err != nil {
		t.Fatalf("trigger tipado válido rechazado: %v", err)
	}
	flow.Edges = flow.Edges[:1]
	if err := Validate(flow); err == nil || !strings.Contains(err.Error(), `salida "image"`) {
		t.Fatalf("faltaba rechazo por puerto image: %v", err)
	}
}

func TestValidateRejectsRawImageConnectedToTextAgent(t *testing.T) {
	flow := &Flow{
		ID: "raw-image", Name: "Imagen cruda",
		Trigger: Trigger{Type: "message", Match: "any", Accepts: []string{"image"}, RouteBy: "content_type"},
		Nodes: []Node{
			{ID: "agent", Kind: "agent", Instruction: "Clasifica", Outputs: []string{"done"}},
			{ID: "done", Kind: "action", Action: "end"},
		},
		Edges: []Edge{
			{ID: "start", Source: "trigger", SourceHandle: "image", Target: "agent"},
			{ID: "done", Source: "agent", SourceHandle: "done", Target: "done"},
		},
	}
	if err := Validate(flow); err == nil || !strings.Contains(err.Error(), "herramienta de transformación") {
		t.Fatalf("se esperaba incompatibilidad de imagen cruda, err=%v", err)
	}
}

func TestValidateOrchestratorReplyPolicy(t *testing.T) {
	base := validAgentFlow()
	base.Nodes[0].AgentRole = "orchestrator"
	base.Nodes[0].Silent = true
	base.Nodes[0].Tools = []NodeTool{{Ref: "data_query", Config: map[string]string{"object": "catalogo"}}}
	if err := Validate(base); err == nil || !strings.Contains(err.Error(), "no puede tener herramientas") {
		t.Fatalf("se esperaba rechazo de herramientas, got %v", err)
	}

	base.Nodes[0].Tools = nil
	base.Nodes[0].Silent = false
	if err := Validate(base); err == nil || !strings.Contains(err.Error(), "limitar su respuesta") {
		t.Fatalf("se esperaba política de ramas, got %v", err)
	}

	base.Nodes[0].ReplyOn = []string{"retry"}
	if err := Validate(base); err != nil {
		t.Fatalf("orquestador conversacional rechazado: %v", err)
	}
	base.Nodes[0].ReplyOn = []string{"inexistente"}
	if err := Validate(base); err == nil || !strings.Contains(err.Error(), "no existe") {
		t.Fatalf("se esperaba rama inexistente, got %v", err)
	}
	base.Nodes[0].ReplyOn = []string{"retry"}
	base.Nodes[0].Silent = true
	if err := Validate(base); err == nil || !strings.Contains(err.Error(), "silencioso") {
		t.Fatalf("se esperaba incompatibilidad con silent, got %v", err)
	}
}

func TestValidateRejectsLoopbackWithoutWait(t *testing.T) {
	flow := validAgentFlow()
	flow.Nodes[2] = Node{
		ID: "retry", Kind: "send", Body: "Reintentando",
	}
	err := Validate(flow)
	if err == nil || !strings.Contains(err.Error(), "ciclo sin un nodo wait") {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateRejectsAutomaticCycleWithoutLoopbackRole(t *testing.T) {
	flow := &Flow{
		ID:      "automatic-cycle",
		Name:    "Ciclo automático",
		Trigger: Trigger{Type: "message", Match: "any"},
		Nodes: []Node{
			{ID: "first", Kind: "action", Action: "set"},
			{ID: "second", Kind: "action", Action: "set"},
		},
		Edges: []Edge{
			{ID: "start", Source: "trigger", Target: "first"},
			{ID: "first-second", Source: "first", Target: "second"},
			{ID: "second-first", Source: "second", Target: "first"},
		},
	}

	err := Validate(flow)
	if err == nil || !strings.Contains(err.Error(), "ciclo sin un nodo wait") {
		t.Fatalf("un ciclo no marcado también debe rechazarse: %v", err)
	}
}

func TestValidateRejectsAutomaticSelfCycle(t *testing.T) {
	flow := &Flow{
		ID:      "self-cycle",
		Name:    "Autociclo",
		Trigger: Trigger{Type: "message", Match: "any"},
		Nodes:   []Node{{ID: "again", Kind: "action", Action: "set"}},
		Edges: []Edge{
			{ID: "start", Source: "trigger", Target: "again"},
			{ID: "again-again", Source: "again", Target: "again"},
		},
	}

	err := Validate(flow)
	if err == nil || !strings.Contains(err.Error(), "ciclo sin un nodo wait") {
		t.Fatalf("un autociclo automático también debe rechazarse: %v", err)
	}
}

func TestValidateAcceptsUnmarkedCycleThatMustPassThroughWait(t *testing.T) {
	flow := validAgentFlow()
	flow.Edges[3].Role = ""

	if err := Validate(flow); err != nil {
		t.Fatalf("el role es presentación; el wait hace seguro al ciclo real: %v", err)
	}
}

func TestValidateRejectsAutomaticSubcycleEvenIfSCCContainsWait(t *testing.T) {
	flow := &Flow{
		ID:      "mixed-cycle",
		Name:    "Ciclo con bypass",
		Trigger: Trigger{Type: "message", Match: "any"},
		Nodes: []Node{
			{ID: "router", Kind: "agent", Instruction: "Decide", Outputs: []string{"automatic", "pause"}, Silent: true},
			{ID: "automatic", Kind: "action", Action: "set"},
			{ID: "pause", Kind: "wait"},
		},
		Edges: []Edge{
			{ID: "start", Source: "trigger", Target: "router"},
			{ID: "to-automatic", Source: "router", SourceHandle: "automatic", Target: "automatic"},
			{ID: "automatic-router", Source: "automatic", Target: "router"},
			{ID: "to-pause", Source: "router", SourceHandle: "pause", Target: "pause"},
			{ID: "pause-router", Source: "pause", Target: "router"},
		},
	}

	err := Validate(flow)
	if err == nil || !strings.Contains(err.Error(), "ciclo sin un nodo wait") {
		t.Fatalf("un SCC con wait no debe ocultar un subciclo automático: %v", err)
	}
}

func TestValidateRejectsLoopbackRoleThatDoesNotCloseCycle(t *testing.T) {
	flow := &Flow{
		ID:      "false-loopback",
		Name:    "Retorno falso",
		Trigger: Trigger{Type: "message", Match: "any"},
		Nodes: []Node{
			{ID: "message", Kind: "send", Body: "Listo"},
			{ID: "done", Kind: "action", Action: "end"},
		},
		Edges: []Edge{
			{ID: "start", Source: "trigger", Target: "message"},
			{ID: "not-a-cycle", Source: "message", Target: "done", Role: "loopback"},
		},
	}

	err := Validate(flow)
	if err == nil || !strings.Contains(err.Error(), "no cierra un ciclo") {
		t.Fatalf("el role loopback sigue exigiendo un ciclo real: %v", err)
	}
}

// Las herramientas del agente no tienen ramas que validar —su resultado vuelve
// al modelo, no a una arista—, pero sí alcance: el motor solo ejecuta las de su
// registro, y la configuración que acota ese alcance es obligatoria. Fallar aquí
// y no al publicar es la diferencia entre un aviso en el bloque y un rechazo.
func TestValidateHerramientasDelAgente(t *testing.T) {
	tests := []struct {
		name  string
		tools []NodeTool
		want  string
	}{
		{
			name:  "válida con su configuración",
			tools: []NodeTool{{Ref: "data_query", Config: map[string]string{"object": "servicios"}}},
		},
		{
			name: "válida con varios catálogos fijos",
			tools: []NodeTool{{Ref: "data_query", Config: map[string]string{
				"objects": "servicios,planes_bawto", "maxLimit": "8",
			}}},
		},
		{
			name: "rechaza objeto simple y múltiple a la vez",
			tools: []NodeTool{{Ref: "data_query", Config: map[string]string{
				"object": "servicios", "objects": "servicios,planes_bawto",
			}}},
			want: "pero no ambos",
		},
		{
			name:  "inexistente",
			tools: []NodeTool{{Ref: "buscar_en_google"}},
			want:  "no implementada",
		},
		{
			name:  "existe pero no es para agentes",
			tools: []NodeTool{{Ref: "data_mutate"}},
			want:  "no está disponible para agentes",
		},
		{
			name:  "sin la configuración obligatoria",
			tools: []NodeTool{{Ref: "data_query"}},
			want:  "requiere Objeto de datos",
		},
		{
			name: "duplicada",
			tools: []NodeTool{
				{Ref: "data_query", Config: map[string]string{"object": "servicios"}},
				{Ref: "data_query", Config: map[string]string{"object": "facturas"}},
			},
			want: "duplicada",
		},
		{
			name: "configuración que la herramienta no admite",
			tools: []NodeTool{{Ref: "data_query", Config: map[string]string{
				"object": "servicios", "limite": "100",
			}}},
			want: "no admite la configuración",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			flow := validAgentFlow()
			flow.Nodes[0].Tools = tc.tools
			err := Validate(flow)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("debería ser válido: %v", err)
			case tc.want != "" && err == nil:
				t.Fatalf("debería fallar con %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("error %q no menciona %q", err, tc.want)
			}
		})
	}
}

// Una herramienta puede declarar ambos consumidores —`data_query` los declara—,
// pero el que solo vale para uno debe rechazarse en el otro. Desde que se retiró
// `search_data` no queda ninguna solo-agéntica, así que la prueba viva es la del
// sentido contrario: `data_mutate` escribe y jamás puede ofrecérsele al modelo.
//
// La rama simétrica (`!spec.ForGraph`) sigue en `validate.go` sin herramienta que
// la ejerza. Se conserva porque la siguiente tool de solo-lectura para el modelo
// la necesitará; si algún día se decide que ningún caso la justifica, se borra el
// código, no solo la prueba.
func TestValidateBloqueToolRechazaHerramientaDeEscrituraEnUnAgente(t *testing.T) {
	flow := validAgentFlow()
	flow.Nodes[0].Tools = []NodeTool{{Ref: "data_mutate"}}
	err := Validate(flow)
	if err == nil || !strings.Contains(err.Error(), "no está disponible para agentes") {
		t.Fatalf("se esperaba el rechazo por consumidor equivocado, got %v", err)
	}
}

func TestValidateDataMutateRequiresSafeDeclarativeScope(t *testing.T) {
	valid := &Flow{ID: "f", Name: "F", Trigger: Trigger{Type: "message", Match: "any"}, Nodes: []Node{
		{ID: "mutate", Kind: "tool", ToolRef: "data_mutate", Args: map[string]string{
			"object": "cobros", "operation": "upsert", "matchField": "operacion",
			"matchValue": "{receipt.operationCode}", "field.operacion": "{receipt.operationCode}",
			"idempotencyKey": "payment:{input.id}", "linkCurrentContact": "true",
		}},
		{ID: "ok", Kind: "action", Action: "end"}, {ID: "error", Kind: "action", Action: "end"},
	}, Edges: []Edge{
		{ID: "start", Source: "trigger", Target: "mutate"},
		{ID: "ok", Source: "mutate", SourceHandle: "ok", Target: "ok"},
		{ID: "error", Source: "mutate", SourceHandle: "error", Target: "error"},
	}}
	if err := Validate(valid); err != nil {
		t.Fatalf("data_mutate válido rechazado: %v", err)
	}
	invalid := *valid
	invalid.Nodes = append([]Node(nil), valid.Nodes...)
	invalid.Nodes[0].Args = map[string]string{
		"object": "{input.object}", "operation": "create", "field.valor": "1", "idempotencyKey": "k",
	}
	if err := Validate(&invalid); err == nil || !strings.Contains(err.Error(), "objeto fijo") {
		t.Fatalf("se esperaba rechazar objeto dinámico: %v", err)
	}
}

func TestValidateSubscriptionActivateRequiresBoundSaleData(t *testing.T) {
	valid := &Flow{ID: "f", Name: "F", Trigger: Trigger{Type: "message", Match: "any"}, Nodes: []Node{
		{ID: "activate", Kind: "tool", ToolRef: "subscription_activate", Args: map[string]string{
			"activationCode": "{sale.organizationCode}", "planKey": "{sale.planKey}",
			"billingCycle": "{sale.billingCycle}", "paymentRecordId": "{payment.recordId}",
			"phone": "{contact_phone}", "idempotencyKey": "subscription:{payment.recordId}",
		}},
		{ID: "ok", Kind: "action", Action: "end"}, {ID: "error", Kind: "action", Action: "end"},
	}, Edges: []Edge{
		{ID: "start", Source: "trigger", Target: "activate"},
		{ID: "ok", Source: "activate", SourceHandle: "ok", Target: "ok"},
		{ID: "error", Source: "activate", SourceHandle: "error", Target: "error"},
	}}
	if err := Validate(valid); err != nil {
		t.Fatalf("subscription_activate válido rechazado: %v", err)
	}
	invalid := *valid
	invalid.Nodes = append([]Node(nil), valid.Nodes...)
	invalid.Nodes[0].Args = map[string]string{"activationCode": "ABC"}
	if err := Validate(&invalid); err == nil || !strings.Contains(err.Error(), "planKey") {
		t.Fatalf("se esperaba rechazar venta incompleta: %v", err)
	}
}

// La frontera de data_query es asimétrica a propósito: los valores pueden venir
// de una variable, el resto no. Si el objeto, el campo o el operador fueran
// interpolables, un mensaje del cliente podría elegir qué tabla se lee.
func TestValidateDataQuerySeparaConfiguracionDeValores(t *testing.T) {
	base := func(args map[string]string) *Flow {
		return &Flow{ID: "f", Name: "F", Trigger: Trigger{Type: "message", Match: "any"}, Nodes: []Node{
			{ID: "read", Kind: "tool", ToolRef: "data_query", Args: args},
			{ID: "ok", Kind: "action", Action: "end"}, {ID: "error", Kind: "action", Action: "end"},
		}, Edges: []Edge{
			{ID: "start", Source: "trigger", Target: "read"},
			{ID: "ok", Source: "read", SourceHandle: "ok", Target: "ok"},
			{ID: "error", Source: "read", SourceHandle: "error", Target: "error"},
		}}
	}

	valid := base(map[string]string{
		"object": "segmentos", "fields": "clave,contexto,ruta",
		"where.1.field": "clave", "where.1.op": "eq", "where.1.value": "{perfil.first.data.segmento_key}",
		"where.2.field": "activo", "where.2.op": "eq", "where.2.value": "true",
		"orderBy": "prioridad", "orderDir": "desc", "limit": "1",
	})
	if err := Validate(valid); err != nil {
		t.Fatalf("data_query válido rechazado: %v", err)
	}

	// Un bloque sin condiciones lee la tabla entera acotada por limit; es legítimo.
	if err := Validate(base(map[string]string{"object": "servicios", "limit": "5"})); err != nil {
		t.Fatalf("data_query sin condiciones rechazado: %v", err)
	}

	for name, tc := range map[string]struct {
		args map[string]string
		want string
	}{
		"objeto dinámico": {
			map[string]string{"object": "{input.text}"}, "objeto fijo",
		},
		"campo dinámico": {
			map[string]string{"object": "s", "where.1.field": "{input.text}", "where.1.value": "x"}, "field fijo",
		},
		"operador dinámico": {
			map[string]string{"object": "s", "where.1.field": "clave", "where.1.op": "{input.text}", "where.1.value": "x"}, "op fijo",
		},
		"operador inventado": {
			map[string]string{"object": "s", "where.1.field": "clave", "where.1.op": "regex", "where.1.value": "x"}, "operador",
		},
		"campos dinámicos": {
			map[string]string{"object": "s", "fields": "{input.text}"}, "variables en fields",
		},
		"argumento desconocido": {
			map[string]string{"object": "s", "sql": "SELECT 1"}, "no admite el argumento",
		},
		"condición sin campo": {
			map[string]string{"object": "s", "where.1.op": "eq", "where.1.value": "x"}, "no declara campo",
		},
		"índice fuera de rango": {
			map[string]string{"object": "s", "where.9.field": "clave"}, "no admite el argumento",
		},
		"limit fuera de rango": {
			map[string]string{"object": "s", "limit": "500"}, "entre 1 y 50",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := Validate(base(tc.args))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("esperaba %q, got %v", tc.want, err)
			}
		})
	}
}

// La asimetría del catálogo es la misma que la de data_query y es lo que impide
// que el mensaje del cliente decida a qué tienda se pregunta: **solo `query`
// interpola variables**; conexión, categoría, orden y tope los fija el autor.
func TestValidateCatalogSearchSeparaDestinoDeValores(t *testing.T) {
	base := func(args map[string]string) *Flow {
		return &Flow{ID: "f", Name: "F", Trigger: Trigger{Type: "message", Match: "any"}, Nodes: []Node{
			{ID: "buscar", Kind: "tool", ToolRef: "catalog_search", Args: args},
			{ID: "ok", Kind: "action", Action: "end"}, {ID: "error", Kind: "action", Action: "end"},
		}, Edges: []Edge{
			{ID: "start", Source: "trigger", Target: "buscar"},
			{ID: "ok", Source: "buscar", SourceHandle: "ok", Target: "ok"},
			{ID: "error", Source: "buscar", SourceHandle: "error", Target: "error"},
		}}
	}

	valid := base(map[string]string{
		"connection": "meudim", "query": "{input.text}", "categoryId": "3",
		"includeDescendants": "true", "sort": "price-asc", "limit": "5",
		"urlTemplate": "https://sistemuino.com/productos/{slug}",
	})
	if err := Validate(valid); err != nil {
		t.Fatalf("catalog_search válido rechazado: %v", err)
	}

	for name, tc := range map[string]struct {
		args map[string]string
		want string
	}{
		"sin conexión": {
			map[string]string{"query": "esp32"}, "requiere una conexión",
		},
		"conexión dinámica": {
			map[string]string{"connection": "{input.text}"}, "conexión fija",
		},
		"categoría dinámica": {
			map[string]string{"connection": "meudim", "categoryId": "{input.text}"}, "categoryId fijo",
		},
		"categoría no numérica": {
			map[string]string{"connection": "meudim", "categoryId": "sensores"}, "numérica",
		},
		"orden inventado": {
			map[string]string{"connection": "meudim", "sort": "price-random"}, "orden",
		},
		"tope por encima del máximo": {
			map[string]string{"connection": "meudim", "limit": "50"}, "como mucho",
		},
		"argumento desconocido": {
			map[string]string{"connection": "meudim", "status": "draft"}, "no admite el argumento",
		},
		"enlace sin TLS": {
			map[string]string{"connection": "meudim", "urlTemplate": "http://tienda.com/{slug}"}, "https",
		},
		"enlace con variable ajena": {
			map[string]string{"connection": "meudim", "urlTemplate": "https://t.com/{input.text}"}, "solo admite",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := Validate(base(tc.args))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("esperaba %q, got %v", tc.want, err)
			}
		})
	}
}

// dataset_query reproduce la misma asimetría que data_query, esta vez sobre un
// dataset externo: connection, resource, fields, sort y cada where.<n>.field/op
// quedan fijos por el autor; solo where.<n>.value y query interpolan.
func TestValidateDatasetQuerySeparaConfiguracionDeValores(t *testing.T) {
	base := func(args map[string]string) *Flow {
		return &Flow{ID: "f", Name: "F", Trigger: Trigger{Type: "message", Match: "any"}, Nodes: []Node{
			{ID: "read", Kind: "tool", ToolRef: "dataset_query", Args: args},
			{ID: "ok", Kind: "action", Action: "end"}, {ID: "error", Kind: "action", Action: "end"},
		}, Edges: []Edge{
			{ID: "start", Source: "trigger", Target: "read"},
			{ID: "ok", Source: "read", SourceHandle: "ok", Target: "ok"},
			{ID: "error", Source: "read", SourceHandle: "error", Target: "error"},
		}}
	}

	valid := base(map[string]string{
		"connection": "erp", "resource": "clientes", "fields": "nombre,saldo",
		"where.1.field": "documento", "where.1.op": "eq", "where.1.value": "{input.text}",
		"sort": "-saldo", "query": "{input.text}", "limit": "5",
	})
	if err := Validate(valid); err != nil {
		t.Fatalf("dataset_query válido rechazado: %v", err)
	}

	// Un recurso anidado (`pedidos/abiertos`) es legítimo; sin condiciones
	// también, igual que data_query.
	if err := Validate(base(map[string]string{"connection": "erp", "resource": "pedidos/abiertos"})); err != nil {
		t.Fatalf("dataset_query sin condiciones rechazado: %v", err)
	}

	for name, tc := range map[string]struct {
		args map[string]string
		want string
	}{
		"sin conexión": {
			map[string]string{"resource": "clientes"}, "requiere una conexión",
		},
		"conexión dinámica": {
			map[string]string{"connection": "{input.text}", "resource": "clientes"}, "conexión fija",
		},
		"sin recurso": {
			map[string]string{"connection": "erp"}, "recurso fijo",
		},
		"recurso dinámico": {
			map[string]string{"connection": "erp", "resource": "{input.text}"}, "recurso fijo",
		},
		"recurso con traversal": {
			map[string]string{"connection": "erp", "resource": "../secreto"}, "letras, números, guiones",
		},
		"campo dinámico": {
			map[string]string{"connection": "erp", "resource": "clientes",
				"where.1.field": "{input.text}", "where.1.value": "x"}, "field fijo",
		},
		"operador dinámico": {
			map[string]string{"connection": "erp", "resource": "clientes",
				"where.1.field": "documento", "where.1.op": "{input.text}", "where.1.value": "x"}, "op fijo",
		},
		"operador inventado": {
			map[string]string{"connection": "erp", "resource": "clientes",
				"where.1.field": "documento", "where.1.op": "regex", "where.1.value": "x"}, "operador",
		},
		"campos dinámicos": {
			map[string]string{"connection": "erp", "resource": "clientes", "fields": "{input.text}"},
			"variables en fields",
		},
		"sort dinámico": {
			map[string]string{"connection": "erp", "resource": "clientes", "sort": "{input.text}"},
			"variables en sort",
		},
		"argumento desconocido": {
			map[string]string{"connection": "erp", "resource": "clientes", "sql": "DROP TABLE x"},
			"no admite el argumento",
		},
		"condición sin campo": {
			map[string]string{"connection": "erp", "resource": "clientes",
				"where.1.op": "eq", "where.1.value": "x"}, "no declara campo",
		},
		"limit fuera de rango": {
			map[string]string{"connection": "erp", "resource": "clientes", "limit": "500"}, "entre 1 y 50",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := Validate(base(tc.args))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("esperaba %q, got %v", tc.want, err)
			}
		})
	}
}

// El agente recibe la misma restricción que en catalog_search: puede redactar
// la búsqueda, nunca la conexión ni el recurso.
func TestValidateDatasetQueryEnUnAgente(t *testing.T) {
	base := func(config map[string]string) *Flow {
		return &Flow{ID: "f", Name: "F", Trigger: Trigger{Type: "message", Match: "any"}, Nodes: []Node{
			{ID: "a", Kind: "agent", Instruction: "consulta", Outputs: []string{"seguir"},
				Tools: []NodeTool{{Ref: "dataset_query", Config: config}}},
			{ID: "fin", Kind: "action", Action: "end"},
		}, Edges: []Edge{
			{ID: "start", Source: "trigger", Target: "a"},
			{ID: "seguir", Source: "a", SourceHandle: "seguir", Target: "fin"},
		}}
	}
	if err := Validate(base(map[string]string{"connection": "erp", "resource": "clientes", "limit": "5"})); err != nil {
		t.Fatalf("agente con dataset rechazado: %v", err)
	}
	if err := Validate(base(map[string]string{"resource": "clientes"})); err == nil {
		t.Fatal("se aceptó un agente con dataset sin conexión")
	}
	if err := Validate(base(map[string]string{"connection": "erp"})); err == nil {
		t.Fatal("se aceptó un agente con dataset sin recurso")
	}
	if err := Validate(base(map[string]string{"connection": "{input.text}", "resource": "clientes"})); err == nil {
		t.Fatal("se aceptó una conexión elegida por una variable")
	}
	if err := Validate(base(map[string]string{"connection": "erp", "resource": "clientes", "limit": "500"})); err == nil {
		t.Fatal("se aceptó un tope por encima del máximo")
	}
	if err := Validate(base(map[string]string{"connection": "erp", "resource": "../secreto"})); err == nil {
		t.Fatal("se aceptó un recurso con traversal")
	}
}

func TestValidateCatalogProductExigeUnSoloIdentificador(t *testing.T) {
	base := func(args map[string]string) *Flow {
		return &Flow{ID: "f", Name: "F", Trigger: Trigger{Type: "message", Match: "any"}, Nodes: []Node{
			{ID: "ver", Kind: "tool", ToolRef: "catalog_product", Args: args},
			{ID: "ok", Kind: "action", Action: "end"}, {ID: "error", Kind: "action", Action: "end"},
		}, Edges: []Edge{
			{ID: "start", Source: "trigger", Target: "ver"},
			{ID: "ok", Source: "ver", SourceHandle: "ok", Target: "ok"},
			{ID: "error", Source: "ver", SourceHandle: "error", Target: "error"},
		}}
	}
	if err := Validate(base(map[string]string{"connection": "meudim", "slug": "{elegido.slug}"})); err != nil {
		t.Fatalf("catalog_product por slug rechazado: %v", err)
	}
	if err := Validate(base(map[string]string{"connection": "meudim"})); err == nil {
		t.Fatal("se aceptó un bloque sin producto")
	}
	if err := Validate(base(map[string]string{"connection": "meudim", "slug": "x", "productId": "7"})); err == nil {
		t.Fatal("se aceptaron los dos identificadores a la vez")
	}
}

// El agente recibe la misma restricción: puede redactar la búsqueda, nunca el
// alcance. Un tope por encima del máximo tampoco pasa por aquí, porque cada
// resultado se reenvía en todas las iteraciones siguientes del turno.
func TestValidateCatalogEnUnAgente(t *testing.T) {
	base := func(config map[string]string) *Flow {
		return &Flow{ID: "f", Name: "F", Trigger: Trigger{Type: "message", Match: "any"}, Nodes: []Node{
			{ID: "a", Kind: "agent", Instruction: "vende", Outputs: []string{"seguir"},
				Tools: []NodeTool{{Ref: "catalog_search", Config: config}}},
			{ID: "fin", Kind: "action", Action: "end"},
		}, Edges: []Edge{
			{ID: "start", Source: "trigger", Target: "a"},
			{ID: "seguir", Source: "a", SourceHandle: "seguir", Target: "fin"},
		}}
	}
	if err := Validate(base(map[string]string{"connection": "meudim", "maxLimit": "5"})); err != nil {
		t.Fatalf("agente con catálogo rechazado: %v", err)
	}
	if err := Validate(base(map[string]string{})); err == nil {
		t.Fatal("se aceptó un agente con catálogo sin conexión")
	}
	if err := Validate(base(map[string]string{"connection": "{input.text}"})); err == nil {
		t.Fatal("se aceptó una conexión elegida por una variable")
	}
	if err := Validate(base(map[string]string{"connection": "meudim", "maxLimit": "40"})); err == nil {
		t.Fatal("se aceptó un tope por encima del máximo")
	}
}

// El bloque que cierra la venta. La clave de idempotencia es obligatoria por la
// misma razón que en data_mutate, elevada al cuadrado: aquí un reintento no
// duplica una fila, duplica un pedido con mercadería y dinero detrás.
func TestValidateOrderCreate(t *testing.T) {
	base := func(args map[string]string) *Flow {
		return &Flow{ID: "f", Name: "F", Trigger: Trigger{Type: "message", Match: "any"}, Nodes: []Node{
			{ID: "pedido", Kind: "tool", ToolRef: "order_create", Args: args},
			{ID: "ok", Kind: "action", Action: "end"}, {ID: "error", Kind: "action", Action: "end"},
		}, Edges: []Edge{
			{ID: "start", Source: "trigger", Target: "pedido"},
			{ID: "ok", Source: "pedido", SourceHandle: "ok", Target: "ok"},
			{ID: "error", Source: "pedido", SourceHandle: "error", Target: "error"},
		}}
	}
	valido := map[string]string{
		"connection": "meudim", "customerEmail": "{perfil.first.data.correo}",
		"customerName": "{contact_name}", "idempotencyKey": "message:{input.id}",
		"item.1.productId": "{elegido.id}", "item.1.quantity": "1",
	}
	if err := Validate(base(valido)); err != nil {
		t.Fatalf("order_create válido rechazado: %v", err)
	}

	for name, tc := range map[string]struct {
		args map[string]string
		want string
	}{
		"sin conexión": {
			map[string]string{"customerEmail": "a@b.c", "idempotencyKey": "k", "item.1.productId": "1", "item.1.quantity": "1"},
			"requiere una conexión",
		},
		"sin correo": {
			map[string]string{"connection": "meudim", "idempotencyKey": "k", "item.1.productId": "1", "item.1.quantity": "1"},
			"correo del comprador",
		},
		"sin idempotencia": {
			map[string]string{"connection": "meudim", "customerEmail": "a@b.c", "item.1.productId": "1", "item.1.quantity": "1"},
			"idempotencyKey",
		},
		"sin líneas": {
			map[string]string{"connection": "meudim", "customerEmail": "a@b.c", "idempotencyKey": "k"},
			"al menos una línea",
		},
		"línea sin cantidad": {
			map[string]string{"connection": "meudim", "customerEmail": "a@b.c", "idempotencyKey": "k", "item.1.productId": "1"},
			"no declara cantidad",
		},
		"cantidad sin producto": {
			map[string]string{"connection": "meudim", "customerEmail": "a@b.c", "idempotencyKey": "k",
				"item.1.productId": "1", "item.1.quantity": "1", "item.2.quantity": "3"},
			"no declara producto",
		},
		// El precio no es un argumento y no debe llegar a serlo: la tienda lo lee
		// de su catálogo y descarta el que se le mande. Aceptarlo aquí haría creer
		// que el flujo controla el importe.
		"precio del flujo": {
			map[string]string{"connection": "meudim", "customerEmail": "a@b.c", "idempotencyKey": "k",
				"item.1.productId": "1", "item.1.quantity": "1", "item.1.unitPrice": "9.90"},
			"no admite el argumento",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := Validate(base(tc.args))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("esperaba %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidatePagosDelPedido(t *testing.T) {
	base := func(ref string, args map[string]string) *Flow {
		return &Flow{ID: "f", Name: "F", Trigger: Trigger{Type: "message", Match: "any"}, Nodes: []Node{
			{ID: "pago", Kind: "tool", ToolRef: ref, Args: args},
			{ID: "ok", Kind: "action", Action: "end"}, {ID: "error", Kind: "action", Action: "end"},
		}, Edges: []Edge{
			{ID: "start", Source: "trigger", Target: "pago"},
			{ID: "ok", Source: "pago", SourceHandle: "ok", Target: "ok"},
			{ID: "error", Source: "pago", SourceHandle: "error", Target: "error"},
		}}
	}
	if err := Validate(base("payment_intent_create", map[string]string{
		"connection": "meudim", "orderId": "{pedido.orderId}",
	})); err != nil {
		t.Fatalf("payment_intent_create válido rechazado: %v", err)
	}
	if err := Validate(base("payment_intent_create", map[string]string{"connection": "meudim"})); err == nil {
		t.Fatal("se aceptó un intent sin pedido")
	}
	if err := Validate(base("payment_intent_create", map[string]string{
		"connection": "meudim", "orderId": "1", "provider": "culqi",
	})); err == nil {
		t.Fatal("se aceptó un proveedor que no existe")
	}

	// La referencia sale del agente visual: receipt.operationCode.
	if err := Validate(base("payment_submit", map[string]string{
		"connection": "meudim", "paymentId": "{cobro.paymentId}", "reference": "{receipt.operationCode}",
		"declaredAmount": "{receipt.amount}", "declaredAt": "{receipt.occurredAt}",
		"channel": "{receipt.provider}", "payerName": "{receipt.payerName}",
		"recipient": "{receipt.recipient}",
	})); err != nil {
		t.Fatalf("payment_submit válido rechazado: %v", err)
	}
	if err := Validate(base("payment_submit", map[string]string{
		"connection": "meudim", "paymentId": "11",
	})); err == nil {
		t.Fatal("se aceptó una declaración sin número de operación")
	}
}

// Las tres son de escritura y ninguna se le ofrece al modelo: un modelo que
// puede crear pedidos puede crear pedidos por error.
func TestValidateVentaNoSeLeOfreceAlAgente(t *testing.T) {
	for _, ref := range []string{"order_create", "payment_intent_create", "payment_submit"} {
		flow := &Flow{ID: "f", Name: "F", Trigger: Trigger{Type: "message", Match: "any"}, Nodes: []Node{
			{ID: "a", Kind: "agent", Instruction: "vende", Outputs: []string{"seguir"},
				Tools: []NodeTool{{Ref: ref}}},
			{ID: "fin", Kind: "action", Action: "end"},
		}, Edges: []Edge{
			{ID: "start", Source: "trigger", Target: "a"},
			{ID: "seguir", Source: "a", SourceHandle: "seguir", Target: "fin"},
		}}
		if err := Validate(flow); err == nil {
			t.Errorf("se le ofreció %q al modelo", ref)
		}
	}
}
