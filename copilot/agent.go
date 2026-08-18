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
	SystemPrompt string               `json:"systemPrompt"`
	Scope        TurnScope            `json:"-"`
	Initial      *InitialModelContext `json:"initial,omitempty"`
	Continuation any                  `json:"-"`
	ToolResults  []FunctionResult     `json:"toolResults,omitempty"`
	Tools        []FunctionDefinition `json:"tools"`
	// Step es el paso del presupuesto que ejecuta esta petición. El proveedor no
	// lo usa para decidir nada: viaja para que los deltas que emite mientras
	// streamea se puedan atribuir al paso correcto en el panel.
	Step            int  `json:"step"`
	RequireTerminal bool `json:"requireTerminal"`
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
	Step int    `json:"step"`
	Name string `json:"name"`
	// Phase separa el ciclo de vida del resultado. Antes ambos compartían campo
	// y el cierre siempre decía "finished", así que una tool que fallaba se
	// pintaba en el panel igual que una que había ido bien.
	Phase      string `json:"phase"`            // started | finished
	Status     string `json:"status,omitempty"` // ok | error, solo en finished
	DurationMs int64  `json:"durationMs,omitempty"`
}

// StreamDelta es el avance del modelo *dentro* de un paso, tal como lo entrega
// el proveedor mientras genera. Es lo que permite que el panel pinte el
// razonamiento token a token en vez de esperar a que el paso entero termine.
//
// Kind "tool" no lleva Content: anuncia que el proveedor abrió un bloque
// tool_use y solo transporta el nombre. Los argumentos de una function call
// nunca salen por aquí, igual que en ActivityEvent.
type StreamDelta struct {
	Step     int    `json:"step"`
	Kind     string `json:"kind"` // thinking | text | tool
	Content  string `json:"content,omitempty"`
	ToolName string `json:"toolName,omitempty"`
}

type activitySinkContextKey struct{}
type deltaSinkContextKey struct{}

// WithDeltaSink instala el consumidor de deltas del proveedor. El provider lo
// invoca desde dentro del bucle de streaming, así que el sink debe ser barato y
// no bloquear: cualquier espera aquí frena la generación del modelo.
func WithDeltaSink(ctx context.Context, sink func(StreamDelta)) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, deltaSinkContextKey{}, sink)
}

func emitDelta(ctx context.Context, delta StreamDelta) {
	if sink, ok := ctx.Value(deltaSinkContextKey{}).(func(StreamDelta)); ok {
		sink(delta)
	}
}

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
