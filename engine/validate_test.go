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
