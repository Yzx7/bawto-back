package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Yzx7/sacs-chatbots/engine"
	"github.com/Yzx7/sacs-chatbots/engine/ai"
	"github.com/Yzx7/sacs-chatbots/env"
)

func TestRunAgentRoutesTextToDeepSeek(t *testing.T) {
	var textCalls, visionCalls atomic.Int32
	con := New(&env.Env{
		TextAgent:   routingTestAgent(t, "deepseek", "deepseek-v4-flash", &textCalls),
		VisionAgent: routingTestAgent(t, "minimax", "MiniMax-M3", &visionCalls),
	})

	result, usage, err := con.runAgent(context.Background(), "org-1", engine.AgentRequest{
		Vars: map[string]string{"input": "hola"}, Outputs: []string{"continuar"}, Silent: true,
	}, nil, nil)
	if err != nil {
		t.Fatalf("runAgent: %v", err)
	}
	if result.Branch != "continuar" || usage.Provider != "deepseek" || usage.Model != "deepseek-v4-flash" {
		t.Fatalf("resultado de texto inesperado: result=%+v usage=%+v", result, usage)
	}
	if textCalls.Load() != 1 || visionCalls.Load() != 0 {
		t.Fatalf("llamadas inesperadas: texto=%d visión=%d", textCalls.Load(), visionCalls.Load())
	}
}

func TestRunAgentRoutesImageCapabilityToMiniMax(t *testing.T) {
	var textCalls, visionCalls atomic.Int32
	con := New(&env.Env{
		TextAgent:   routingTestAgent(t, "deepseek", "deepseek-v4-flash", &textCalls),
		VisionAgent: routingTestAgent(t, "minimax", "MiniMax-M3", &visionCalls),
	})

	result, usage, err := con.runAgent(context.Background(), "org-1", engine.AgentRequest{
		Vars: map[string]string{"input": "mira esto"}, Outputs: []string{"continuar"}, Silent: true,
		Accepts: []string{"text", "image"},
	}, nil, nil)
	if err != nil {
		t.Fatalf("runAgent: %v", err)
	}
	if result.Branch != "continuar" || usage.Provider != "minimax" || usage.Model != "MiniMax-M3" {
		t.Fatalf("resultado visual inesperado: result=%+v usage=%+v", result, usage)
	}
	if textCalls.Load() != 0 || visionCalls.Load() != 1 {
		t.Fatalf("llamadas inesperadas: texto=%d visión=%d", textCalls.Load(), visionCalls.Load())
	}
}

func TestRunAgentDoesNotFallbackToTextWhenVisionIsMissing(t *testing.T) {
	var textCalls atomic.Int32
	con := New(&env.Env{
		TextAgent: routingTestAgent(t, "deepseek", "deepseek-v4-flash", &textCalls),
	})

	_, usage, err := con.runAgent(context.Background(), "org-1", engine.AgentRequest{
		Vars: map[string]string{"input": "imagen"}, Outputs: []string{"continuar"}, Silent: true,
		Accepts: []string{"image"},
	}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "MINIMAX_M3_API_KEY") {
		t.Fatalf("se esperaba error de proveedor visual, got %v", err)
	}
	if usage.Provider != "" || textCalls.Load() != 0 {
		t.Fatalf("hubo fallback silencioso a texto: usage=%+v llamadas=%d", usage, textCalls.Load())
	}
}

func routingTestAgent(t *testing.T, provider, model string, calls *atomic.Int32) *ai.Agent {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"routing-request",
			"type":"message",
			"role":"assistant",
			"model":"` + model + `",
			"content":[{
				"type":"tool_use",
				"id":"routing-tool",
				"name":"select_flow_branch",
				"input":{"branch":"continuar"}
			}],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":1,"output_tokens":1}
		}`))
	}))
	t.Cleanup(srv.Close)
	return ai.NewWithPricing("sk-test", srv.URL, provider, model, ai.Rates{})
}
