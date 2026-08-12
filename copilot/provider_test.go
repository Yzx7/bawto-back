package copilot

import (
	"strings"
	"testing"
)

func TestToolParamsForMapsDefinitions(t *testing.T) {
	defs := AuthoringFunctionDefinitions()
	params := toolParamsFor(defs)
	if len(params) != len(defs) {
		t.Fatalf("toolParamsFor devolvió %d tools para %d definiciones", len(params), len(defs))
	}
	for i, param := range params {
		if param.OfTool == nil {
			t.Fatalf("tool %d sin OfTool", i)
		}
		if param.OfTool.Name != defs[i].Name {
			t.Fatalf("tool %d: nombre %q, esperado %q", i, param.OfTool.Name, defs[i].Name)
		}
		if param.OfTool.InputSchema.ExtraFields["additionalProperties"] != false {
			t.Fatalf("tool %s debe cerrar additionalProperties", defs[i].Name)
		}
	}
}

func TestInitialPromptIncludesRequest(t *testing.T) {
	initial := &InitialModelContext{UserRequest: "crea un flujo de cobros"}
	prompt := initialPrompt(initial)
	if !strings.Contains(prompt, "crea un flujo de cobros") {
		t.Fatalf("el prompt inicial debe incluir la petición del autor: %q", prompt)
	}
	if initialPrompt(nil) == "" {
		t.Fatal("un contexto nulo no debe producir un prompt vacío")
	}
}

func TestReasoningRequestOptions(t *testing.T) {
	if got := reasoningRequestOptions("deepseek", "high"); len(got) != 1 {
		t.Errorf("deepseek debe inyectar thinking deshabilitado, dio %d opciones", len(got))
	}
	if got := reasoningRequestOptions("minimax", "high"); len(got) != 0 {
		t.Errorf("minimax razona por defecto y no debe inyectar nada, dio %d opciones", len(got))
	}
	if got := reasoningRequestOptions("anthropic", "high"); len(got) != 1 {
		t.Errorf("anthropic con effort high debe activar thinking, dio %d opciones", len(got))
	}
	if got := reasoningRequestOptions("anthropic", "none"); len(got) != 0 {
		t.Errorf("anthropic con effort none no debe activar thinking, dio %d opciones", len(got))
	}
}
