package controllers

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/config"
	appenv "github.com/Yzx7/sacs-chatbots/env"
	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
)

func TestDecodeStrictCopilotBody(t *testing.T) {
	var valid struct {
		ExpectedDraftChecksum string `json:"expectedDraftChecksum"`
	}
	if err := decodeStrictCopilotBody([]byte(`{"expectedDraftChecksum":"abc"}`), &valid); err != nil || valid.ExpectedDraftChecksum != "abc" {
		t.Fatalf("body válido rechazado: value=%q err=%v", valid.ExpectedDraftChecksum, err)
	}
	for _, raw := range []string{
		`{"expectedDraftChecksum":"abc","extra":true}`,
		`{"expectedDraftChecksum":"abc"} {}`,
		`not-json`,
	} {
		var target struct {
			ExpectedDraftChecksum string `json:"expectedDraftChecksum"`
		}
		if err := decodeStrictCopilotBody([]byte(raw), &target); err == nil {
			t.Fatalf("body no fail-closed aceptado: %s", raw)
		}
	}
}

func TestFlowCopilotCapabilityQuedaApagadaSinRunner(t *testing.T) {
	con := &Controller{}
	capability := con.flowCopilotCapability()
	if capability.Enabled || capability.ProviderOperational || capability.Reason == "" {
		t.Fatalf("capability insegura sin entorno: %+v", capability)
	}
	con.Env = &appenv.Env{Config: &config.Config{
		CopilotEnabled: true, CopilotAIAPIKey: "secret", CopilotAIProvider: "provider",
		CopilotAIModel: "thinking", CopilotAIBaseURL: "https://provider.invalid",
		CopilotAIReasoningEffort: "high", CopilotAIMaxSteps: 8, CopilotAITimeout: 90 * time.Second,
	}}
	capability = con.flowCopilotCapability()
	if capability.Enabled || capability.ProviderOperational || !strings.Contains(capability.Reason, "runner") {
		t.Fatalf("configuración sin runner se anunció operativa: %+v", capability)
	}
}

func TestFailFlowCopilotSerializaConflictoTipado(t *testing.T) {
	app := fiber.New()
	con := &Controller{}
	app.Get("/", func(c *fiber.Ctx) error {
		return con.failFlowCopilot(c, "test", "bot", &models.FlowCopilotConflictError{
			Code: "proposal_rebase_required", Problem: "hay que rebasar",
			ExpectedChecksum: "old", CurrentChecksum: "new",
		})
	})
	response, err := app.Test(httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != fiber.StatusConflict {
		t.Fatalf("status=%d", response.StatusCode)
	}
	var envelope types.GenRes
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	data, ok := envelope.Data.(map[string]any)
	if !ok || envelope.Ok || data["code"] != "proposal_rebase_required" || data["currentChecksum"] != "new" {
		t.Fatalf("conflicto incompleto: %+v", envelope)
	}
}
