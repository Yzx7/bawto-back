package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Yzx7/sacs-chatbots/authoring"
)

// ModelProvider is intentionally independent from engine/ai. Continuation is
// opaque provider state retained only for this turn, which lets an adapter
// preserve thinking/tool blocks exactly without exposing or persisting them.
type ModelProvider interface {
	Next(context.Context, ModelRequest) (ModelResponse, error)
}

type ModelRequest struct {
	SystemPrompt    string               `json:"systemPrompt"`
	Scope           TurnScope            `json:"-"`
	Initial         *InitialModelContext `json:"initial,omitempty"`
	Continuation    any                  `json:"-"`
	ToolResults     []FunctionResult     `json:"toolResults,omitempty"`
	Tools           []FunctionDefinition `json:"tools"`
	RequireTerminal bool                 `json:"requireTerminal"`
}

type InitialModelContext struct {
	UserRequest            string              `json:"userRequest"`
	FlowOutline            FlowOutline         `json:"flowOutline"`
	SelectedNodeID         string              `json:"selectedNodeId,omitempty"`
	PersistedDraftChecksum string              `json:"persistedDraftChecksum,omitempty"`
	WorkingDraftChecksum   string              `json:"workingDraftChecksum"`
	CandidateChecksum      string              `json:"candidateChecksum"`
	EditorRevision         string              `json:"editorRevision,omitempty"`
	SessionSummary         string              `json:"sessionSummary,omitempty"`
	RecentConversation     []ConversationEntry `json:"recentConversation,omitempty"`
	CatalogHash            string              `json:"catalogHash"`
	ResourceHash           string              `json:"resourceHash,omitempty"`
}

type ConversationEntry struct {
	Role    string `json:"role"` // user | assistant
	Content string `json:"content"`
}

type FunctionCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ModelUsage struct {
	Provider                    string  `json:"provider,omitempty"`
	Model                       string  `json:"model,omitempty"`
	ProviderRequestID           string  `json:"providerRequestId,omitempty"`
	Step                        int     `json:"step"`
	InputTokens                 int64   `json:"inputTokens,omitempty"`
	OutputTokens                int64   `json:"outputTokens,omitempty"`
	CacheReadInputTokens        int64   `json:"cacheReadInputTokens,omitempty"`
	CacheWriteInputTokens       int64   `json:"cacheWriteInputTokens,omitempty"`
	InputCostPerMillionUSD      float64 `json:"inputCostPerMillionUsd,omitempty"`
	OutputCostPerMillionUSD     float64 `json:"outputCostPerMillionUsd,omitempty"`
	CacheReadCostPerMillionUSD  float64 `json:"cacheReadCostPerMillionUsd,omitempty"`
	CacheWriteCostPerMillionUSD float64 `json:"cacheWriteCostPerMillionUsd,omitempty"`
	CostUSD                     float64 `json:"costUsd,omitempty"`
}

type ModelResponse struct {
	// Continuation is handed back to the same provider on the next step and is
	// never copied into TurnResult, logs or tool traces.
	Continuation any            `json:"-"`
	Calls        []FunctionCall `json:"calls"`
	Text         string         `json:"text,omitempty"`
	Thought      string         `json:"thought,omitempty"`
	Usage        ModelUsage     `json:"usage"`
}

type FunctionResult struct {
	CallID  string          `json:"callId"`
	Name    string          `json:"name"`
	Output  json.RawMessage `json:"output"`
	IsError bool            `json:"isError,omitempty"`
}

type TurnScope struct {
	OrganizationID string `json:"organizationId"`
	BotID          string `json:"botId"`
	FlowID         string `json:"flowId"`
}

type TurnRequest struct {
	Scope                             TurnScope           `json:"scope"`
	UserRequest                       string              `json:"userRequest"`
	CurrentFlow                       json.RawMessage     `json:"currentFlow"`
	StartingCandidate                 json.RawMessage     `json:"startingCandidate,omitempty"`
	ExpectedStartingCandidateChecksum string              `json:"expectedStartingCandidateChecksum,omitempty"`
	SelectedNodeID                    string              `json:"selectedNodeId,omitempty"`
	PersistedDraftChecksum            string              `json:"persistedDraftChecksum,omitempty"`
	EditorRevision                    string              `json:"editorRevision,omitempty"`
	SessionSummary                    string              `json:"sessionSummary,omitempty"`
	RecentConversation                []ConversationEntry `json:"recentConversation,omitempty"`
	Resources                         ResourceBundle      `json:"resources"`
}

type TerminalMode string

const (
	TerminalQuestion    TerminalMode = "question"
	TerminalExplanation TerminalMode = "explanation"
	TerminalProposal    TerminalMode = "proposal"
)

type SubmitProposalInput struct {
	Mode                      TerminalMode            `json:"mode"`
	Response                  string                  `json:"response"`
	Assumptions               []string                `json:"assumptions,omitempty"`
	PendingDecisions          []string                `json:"pendingDecisions,omitempty"`
	IntentSummary             string                  `json:"intentSummary,omitempty"`
	Warnings                  []string                `json:"warnings,omitempty"`
	ExpectedCandidateChecksum string                  `json:"expectedCandidateChecksum,omitempty"`
	Playbooks                 []authoring.ArtifactRef `json:"playbooks,omitempty"`
}

type ProposalCandidate struct {
	Candidate                 json.RawMessage           `json:"candidate"`
	BaseChecksum              string                    `json:"baseChecksum"`
	StartingCandidateChecksum string                    `json:"startingCandidateChecksum,omitempty"`
	CandidateChecksum         string                    `json:"candidateChecksum"`
	Diff                      authoring.FlowDiff        `json:"diff"`
	Operations                []authoring.FlowOperation `json:"operations"`
	Diagnostics               []authoring.Diagnostic    `json:"diagnostics,omitempty"`
	KnowledgeBundleHashes     map[string]string         `json:"knowledgeBundleHashes,omitempty"`
	PlaybookVersions          []authoring.ArtifactRef   `json:"playbookVersions,omitempty"`
	CatalogHash               string                    `json:"catalogHash"`
	ResourceHash              string                    `json:"resourceHash,omitempty"`
}

type ToolTrace struct {
	Step    int    `json:"step"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	CallID  string `json:"callId,omitempty"`
	Thought string `json:"thought,omitempty"`
}

type ActivityEvent struct {
	Step   int    `json:"step"`
	Name   string `json:"name"`
	Status string `json:"status"` // started | finished
}

type ThoughtEvent struct {
	Step    int    `json:"step"`
	Content string `json:"content"`
}

type activitySinkContextKey struct{}
type thoughtSinkContextKey struct{}

// WithActivitySink streams allowlisted function names and lifecycle only.
func WithActivitySink(ctx context.Context, sink func(ActivityEvent)) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, activitySinkContextKey{}, sink)
}

func emitActivity(ctx context.Context, event ActivityEvent) {
	if sink, ok := ctx.Value(activitySinkContextKey{}).(func(ActivityEvent)); ok {
		sink(event)
	}
}

// WithThoughtSink streams thinking and reasoning blocks emitted by frontier models.
func WithThoughtSink(ctx context.Context, sink func(ThoughtEvent)) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, thoughtSinkContextKey{}, sink)
}

func emitThought(ctx context.Context, event ThoughtEvent) {
	if sink, ok := ctx.Value(thoughtSinkContextKey{}).(func(ThoughtEvent)); ok {
		sink(event)
	}
}

type TurnUsage struct {
	Requests              int     `json:"requests"`
	InputTokens           int64   `json:"inputTokens"`
	OutputTokens          int64   `json:"outputTokens"`
	CacheReadInputTokens  int64   `json:"cacheReadInputTokens,omitempty"`
	CacheWriteInputTokens int64   `json:"cacheWriteInputTokens,omitempty"`
	CostUSD               float64 `json:"costUsd"`
}

type usageRecorderContextKey struct{}

// WithUsageRecorder installs a per-provider-response hook. The loop invokes
// it immediately after every successful Next, before interpreting the output,
// so a later failure or cancellation cannot erase already-incurred usage.
func WithUsageRecorder(ctx context.Context, recorder func(ModelUsage)) context.Context {
	if recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, usageRecorderContextKey{}, recorder)
}

func recordModelUsage(ctx context.Context, usage ModelUsage) {
	if recorder, ok := ctx.Value(usageRecorderContextKey{}).(func(ModelUsage)); ok {
		recorder(usage)
	}
}

type TurnResult struct {
	Terminal SubmitProposalInput `json:"terminal"`
	Proposal *ProposalCandidate  `json:"proposal,omitempty"`
	Trace    []ToolTrace         `json:"trace,omitempty"`
	Thought  string              `json:"thought,omitempty"`
	Usage    TurnUsage           `json:"usage"`
}

type RunnerConfig struct {
	MaxSteps               int
	MaxOperations          int
	MaxToolResultBytes     int
	MaxIdenticalCalls      int
	InvalidTerminalRetries int
	Timeout                time.Duration
}

func DefaultRunnerConfig() RunnerConfig {
	return RunnerConfig{
		MaxSteps: 8, MaxOperations: 60, MaxToolResultBytes: 128 << 10,
		MaxIdenticalCalls: 2, InvalidTerminalRetries: 1, Timeout: 90 * time.Second,
	}
}

var (
	ErrStepBudgetExceeded = errors.New("copilot authoring step budget exceeded")
	ErrRepeatedToolCall   = errors.New("copilot repeated identical tool call")
	ErrInvalidTerminal    = errors.New("copilot invalid terminal")
)

type Agent struct {
	Provider ModelProvider
	Config   RunnerConfig
}

func (agent *Agent) RunTurn(ctx context.Context, request TurnRequest) (*TurnResult, error) {
	return runTurn(ctx, agent.Provider, normalizedRunnerConfig(agent.Config), request)
}

func normalizedRunnerConfig(config RunnerConfig) RunnerConfig {
	defaults := DefaultRunnerConfig()
	if config.MaxSteps <= 0 {
		config.MaxSteps = defaults.MaxSteps
	}
	if config.MaxOperations <= 0 {
		config.MaxOperations = defaults.MaxOperations
	}
	if config.MaxToolResultBytes <= 0 {
		config.MaxToolResultBytes = defaults.MaxToolResultBytes
	}
	if config.MaxIdenticalCalls <= 0 {
		config.MaxIdenticalCalls = defaults.MaxIdenticalCalls
	}
	if config.InvalidTerminalRetries < 0 {
		config.InvalidTerminalRetries = defaults.InvalidTerminalRetries
	}
	if config.Timeout <= 0 {
		config.Timeout = defaults.Timeout
	}
	return config
}

const authoringSystemPrompt = `Eres el Copilot privado de autoría de flujos. Diseñas sobre una copia efímera del grafo visible.
Usa únicamente las function calls declaradas. Inspecciona catálogos y recursos solo cuando sea necesario; no inventes tablas, campos, conexiones, plantillas, variables, node kinds ni runtime tools.
Toda mutación usa apply_flow_operations con expectedCandidateChecksum:
- addNode: {alias: "alias_local", kind: "tipo", set: { ...campos... }}. Las propiedades del nodo van SIEMPRE dentro del objeto "set" (ej: send usa set.body, agent usa set.instruction y set.outputs, action usa set.action='end'|'handoff'|'set', router usa set.cases).
- connectNodes: {alias: "alias_arista", source: {id/alias}, target: {id/alias}, sourceHandle: "puerto_salida"}. Puerto: 'out' (send/wait/action), 'ok'/'error' (agent/tool), 'default'/'caso_id' (router).
Pregunta cuando falte una decisión material. Planifica y aplica las operaciones en lotes consolidados para avanzar eficientemente dentro del presupuesto de pasos.
Termina siempre con submit_proposal. question y explanation descartan cualquier workspace mutado; proposal requiere checksum exacto y validación determinista.
No reveles chain-of-thought. Expón solo respuesta, supuestos, decisiones pendientes, riesgos y operaciones verificables.`
