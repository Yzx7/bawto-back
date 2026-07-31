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
