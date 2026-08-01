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
			tools: []NodeTool{{Ref: "search_data", Config: map[string]string{"object": "servicios"}}},
		},
		{
			name:  "inexistente",
			tools: []NodeTool{{Ref: "buscar_en_google"}},
			want:  "no implementada",
		},
		{
			name:  "existe pero no es para agentes",
			tools: []NodeTool{{Ref: "record_payment_receipt"}},
			want:  "no está disponible para agentes",
		},
		{
			name:  "sin la configuración obligatoria",
			tools: []NodeTool{{Ref: "search_data"}},
			want:  "requiere Objeto de datos",
		},
		{
			name: "duplicada",
			tools: []NodeTool{
				{Ref: "search_data", Config: map[string]string{"object": "servicios"}},
				{Ref: "search_data", Config: map[string]string{"object": "facturas"}},
			},
			want: "duplicada",
		},
		{
			name: "configuración que la herramienta no admite",
			tools: []NodeTool{{Ref: "search_data", Config: map[string]string{
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

// El bloque `tool` del grafo y el agente no comparten catálogo: una herramienta
// pensada para que la llame el modelo no puede colgarse de una arista.
func TestValidateBloqueToolRechazaHerramientaDeAgente(t *testing.T) {
	flow := validAgentFlow()
	flow.Nodes = append(flow.Nodes, Node{ID: "t1", Kind: "tool", ToolRef: "search_data"})
	flow.Edges = append(flow.Edges,
		Edge{ID: "ok-t1", Source: "ok", Target: "t1"},
		Edge{ID: "t1-ok", Source: "t1", SourceHandle: "ok", Target: "ok"},
		Edge{ID: "t1-err", Source: "t1", SourceHandle: "error", Target: "ok"},
	)
	err := Validate(flow)
	if err == nil || !strings.Contains(err.Error(), "solo puede usarla un agente") {
		t.Fatalf("se esperaba el rechazo por consumidor equivocado, got %v", err)
	}
}
