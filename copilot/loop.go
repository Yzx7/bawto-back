package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Yzx7/sacs-chatbots/authoring"
	"github.com/Yzx7/sacs-chatbots/engine"
)

type turnWorkspace struct {
	base                      json.RawMessage
	baseChecksum              string
	startingCandidateChecksum string
	candidate                 json.RawMessage
	candidateChecksum         string
	operations                []authoring.FlowOperation
	resources                 ResourceBundle
	maxOperations             int
}

type applyFlowOperationsInput struct {
	ExpectedCandidateChecksum string                    `json:"expectedCandidateChecksum"`
	Operations                []authoring.FlowOperation `json:"operations"`
}

type applyFlowOperationsOutput struct {
	CandidateChecksum string                 `json:"candidateChecksum"`
	AliasToNodeID     map[string]string      `json:"aliasToNodeId,omitempty"`
	AliasToEdgeID     map[string]string      `json:"aliasToEdgeId,omitempty"`
	Diff              authoring.FlowDiff     `json:"diff"`
	Diagnostics       []authoring.Diagnostic `json:"diagnostics,omitempty"`
}

type functionExecutionError struct {
	Code    string
	Message string
	Details any
}

func (err *functionExecutionError) Error() string { return err.Message }

func newTurnWorkspace(request TurnRequest, maxOperations int) (*turnWorkspace, error) {
	_, baseChecksum, err := engine.CanonicalChecksum(request.CurrentFlow)
	if err != nil {
		return nil, fmt.Errorf("currentFlow inválido: %w", err)
	}
	candidate := append(json.RawMessage(nil), request.CurrentFlow...)
	startingChecksum := ""
	if len(request.StartingCandidate) > 0 {
		if strings.TrimSpace(request.ExpectedStartingCandidateChecksum) == "" {
			return nil, fmt.Errorf("expectedStartingCandidateChecksum es obligatorio al revisar una propuesta")
		}
		_, actual, err := engine.CanonicalChecksum(request.StartingCandidate)
		if err != nil {
			return nil, fmt.Errorf("startingCandidate inválido: %w", err)
		}
		if actual != request.ExpectedStartingCandidateChecksum {
			return nil, &authoring.CandidateChecksumMismatchError{Expected: request.ExpectedStartingCandidateChecksum, Actual: actual}
		}
		candidate = append(json.RawMessage(nil), request.StartingCandidate...)
		startingChecksum = actual
	}
	_, candidateChecksum, err := engine.CanonicalChecksum(candidate)
	if err != nil {
		return nil, err
	}
	return &turnWorkspace{
		base: append(json.RawMessage(nil), request.CurrentFlow...), baseChecksum: baseChecksum,
		startingCandidateChecksum: startingChecksum, candidate: candidate, candidateChecksum: candidateChecksum,
		resources: request.Resources, maxOperations: maxOperations,
	}, nil
}

func runTurn(ctx context.Context, provider ModelProvider, config RunnerConfig, request TurnRequest) (*TurnResult, error) {
	if provider == nil {
		return nil, fmt.Errorf("model provider es obligatorio")
	}
	workspace, err := newTurnWorkspace(request, config.MaxOperations)
	if err != nil {
		return nil, err
	}
	outline, err := flowOutline(workspace.candidate)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, config.Timeout)
	defer cancel()

	initial := &InitialModelContext{
		UserRequest: request.UserRequest, FlowOutline: outline, SelectedNodeID: request.SelectedNodeID,
		PersistedDraftChecksum: request.PersistedDraftChecksum, WorkingDraftChecksum: workspace.baseChecksum,
		CandidateChecksum: workspace.candidateChecksum, EditorRevision: request.EditorRevision,
		SessionSummary: boundedText(request.SessionSummary, 4000), RecentConversation: boundedConversation(request.RecentConversation),
		CatalogHash: authoring.CatalogHash(), ResourceHash: request.Resources.Hash,
	}
	definitions := AuthoringFunctionDefinitions()
	allowedFunctions := make(map[string]bool, len(definitions))
	for _, definition := range definitions {
		allowedFunctions[definition.Name] = true
	}
	modelRequest := ModelRequest{
		SystemPrompt: authoringSystemPrompt, Scope: request.Scope, Initial: initial, Tools: definitions,
	}
	result := &TurnResult{}
	repeated := make(map[string]int)
	invalidTerminalCount := 0
	for step := 1; step <= config.MaxSteps; step++ {
		modelRequest.RequireTerminal = step == config.MaxSteps
		modelRequest.Step = step
		response, err := provider.Next(ctx, modelRequest)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return &TurnResult{
					Terminal: SubmitProposalInput{
						Mode:     TerminalQuestion,
						Response: "El procesamiento de este turno tomó más tiempo del límite configurado mientras diseñaba el flujo. ¿Deseas que continúe en el siguiente mensaje?",
						Warnings: []string{"Se alcanzó el tiempo límite de ejecución en este turno."},
					},
					Trace: result.Trace,
					Usage: result.Usage,
				}, nil
			}
			return nil, err
		}
		response.Usage.Step = step
		recordModelUsage(ctx, response.Usage)
		accumulateUsage(&result.Usage, response.Usage)
		// El razonamiento ya viajó al panel como deltas mientras el proveedor lo
		// generaba. Aquí solo se acumula para persistirlo con el turno.
		if response.Thought != "" {
			if result.Thought != "" {
				result.Thought += "\n\n"
			}
			result.Thought += response.Thought
		}
		if len(response.Calls) == 0 {
			if step == config.MaxSteps || invalidTerminalCount >= config.InvalidTerminalRetries {
				text := strings.TrimSpace(response.Text)
				if text == "" {
					text = "He alcanzado el límite de pasos de este turno mientras diseñaba el flujo. ¿Deseas que continúe en el siguiente mensaje?"
				}
				result.Terminal = SubmitProposalInput{
					Mode:     TerminalQuestion,
					Response: text,
					Warnings: []string{"Se alcanzó el límite de pasos presupuestados en este turno."},
				}
				return result, nil
			}
			invalidTerminalCount++
			modelRequest.Initial = nil
			modelRequest.Continuation = response.Continuation
			modelRequest.ToolResults = []FunctionResult{errorFunctionResult("", ToolSubmitProposal, "missing_terminal", "debes terminar con submit_proposal", nil)}
			continue
		}

		terminalIndex := -1
		for index, call := range response.Calls {
			if call.Name == ToolSubmitProposal {
				terminalIndex = index
			}
		}
		if terminalIndex >= 0 {
			if len(response.Calls) != 1 {
				return nil, fmt.Errorf("%w: submit_proposal debe ser la única llamada del paso", ErrInvalidTerminal)
			}
			call := response.Calls[0]
			emitActivity(ctx, ActivityEvent{Step: step, Name: call.Name, Phase: "started"})
			startedAt := time.Now()
			terminal, proposal, terminalErr := workspace.submit(call.Arguments)
			status := "ok"
			if terminalErr != nil {
				status = "error"
			}
			emitActivity(ctx, ActivityEvent{Step: step, Name: call.Name, Phase: "finished",
				Status: status, DurationMs: time.Since(startedAt).Milliseconds()})
			result.Trace = append(result.Trace, ToolTrace{Step: step, Name: call.Name, Status: status, CallID: call.ID})
			if terminalErr == nil {
				result.Terminal = terminal
				result.Proposal = proposal
				return result, nil
			}
			if step == config.MaxSteps || invalidTerminalCount >= config.InvalidTerminalRetries {
				msg := strings.TrimSpace(terminal.Response)
				if msg == "" {
					msg = "He avanzado en la preparación de la propuesta, pero se requieren ajustes adicionales en las ramas y conexiones. ¿Deseas que continúe en el siguiente mensaje?"
				}
				result.Terminal = SubmitProposalInput{
					Mode:     TerminalQuestion,
					Response: msg,
					Warnings: []string{"No se pudo aplicar la propuesta automáticamente: " + terminalErr.Error()},
				}
				return result, nil
			}
			invalidTerminalCount++
			modelRequest.Initial = nil
			modelRequest.Continuation = response.Continuation
			modelRequest.ToolResults = []FunctionResult{errorFunctionResult(call.ID, call.Name, "invalid_terminal", terminalErr.Error(), nil)}
			continue
		}
		if modelRequest.RequireTerminal {
			text := strings.TrimSpace(response.Text)
			if text == "" {
				text = "He avanzado en la preparación de los nodos del flujo y alcancé el límite de pasos de este turno. ¿Deseas que continúe conectando y validando el flujo en el siguiente mensaje?"
			}
			result.Terminal = SubmitProposalInput{
				Mode:     TerminalQuestion,
				Response: text,
				Warnings: []string{"Se alcanzó el límite de pasos presupuestados en este turno."},
			}
			return result, nil
		}

		toolResults := make([]FunctionResult, 0, len(response.Calls))
		for _, call := range response.Calls {
			if !allowedFunctions[call.Name] {
				toolResults = append(toolResults, errorFunctionResult(call.ID, call.Name, "unknown_tool",
					fmt.Sprintf("La herramienta %q no existe en el catálogo de autoría. Usa únicamente herramientas declaradas (como apply_flow_operations) o submit_proposal (con mode=question, explanation o proposal).", call.Name), nil))
				continue
			}
			signature, err := functionCallSignature(call)
			if err != nil {
				return nil, err
			}
			repeated[signature]++
			if repeated[signature] > config.MaxIdenticalCalls {
				if repeated[signature] == config.MaxIdenticalCalls+1 {
					functionResult := errorFunctionResult(call.ID, call.Name, "repeated_call",
						"Has repetido esta misma llamada con argumentos idénticos. Corrige los parámetros o envía la propuesta con submit_proposal.", nil)
					toolResults = append(toolResults, functionResult)
					continue
				}
				if text := strings.TrimSpace(response.Text); text != "" {
					result.Terminal = SubmitProposalInput{
						Mode:     TerminalExplanation,
						Response: text,
						Warnings: []string{"El asistente repitió la misma consulta sin avanzar."},
					}
					return result, nil
				}
				result.Terminal = SubmitProposalInput{
					Mode:     TerminalExplanation,
					Response: "No pude completar la modificación porque entré en un bucle repitiendo la misma operación. Por favor, intenta pedir el cambio especificando los nodos o acciones paso a paso.",
					Warnings: []string{"Operación repetida: " + call.Name},
				}
				return result, nil
			}
			emitActivity(ctx, ActivityEvent{Step: step, Name: call.Name, Phase: "started"})
			startedAt := time.Now()
			output, executeErr := workspace.execute(call)
			status := "ok"
			var functionResult FunctionResult
			if executeErr != nil {
				status = "error"
				var typed *functionExecutionError
				if errors.As(executeErr, &typed) {
					functionResult = errorFunctionResult(call.ID, call.Name, typed.Code, typed.Message, typed.Details)
				} else {
					functionResult = errorFunctionResult(call.ID, call.Name, "tool_error", executeErr.Error(), nil)
				}
			} else {
				functionResult, err = successfulFunctionResult(call.ID, call.Name, output, config.MaxToolResultBytes)
				if err != nil {
					status = "error"
					functionResult = errorFunctionResult(call.ID, call.Name, "tool_result_too_large", err.Error(), nil)
				}
			}
			emitActivity(ctx, ActivityEvent{Step: step, Name: call.Name, Phase: "finished",
				Status: status, DurationMs: time.Since(startedAt).Milliseconds()})
			result.Trace = append(result.Trace, ToolTrace{Step: step, Name: call.Name, Status: status, CallID: call.ID})
			toolResults = append(toolResults, functionResult)
		}
		modelRequest.Initial = nil
		modelRequest.Continuation = response.Continuation
		modelRequest.ToolResults = toolResults
	}
	return &TurnResult{
		Terminal: SubmitProposalInput{
			Mode:     TerminalQuestion,
			Response: "He alcanzado el límite de pasos de este turno mientras diseñaba el flujo. ¿Deseas que continúe en el siguiente mensaje?",
			Warnings: []string{"Se alcanzó el límite de pasos presupuestados en este turno."},
		},
		Trace: result.Trace,
		Usage: result.Usage,
	}, nil
}

func (workspace *turnWorkspace) execute(call FunctionCall) (any, error) {
	switch call.Name {
	case ToolGetFlowOutline:
		return flowOutline(workspace.candidate)
	case ToolGetNodes:
		var input struct {
			IDs []string `json:"ids"`
		}
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return nil, err
		}
		return workspace.getNodes(input.IDs)
	case ToolGetAuthoringCatalog:
		return map[string]any{"nodes": authoring.NodeCatalog(), "catalogHash": authoring.CatalogHash()}, nil
	case ToolGetRuntimeToolCatalog:
		return authoring.RuntimeToolCatalog(), nil
	case ToolListDataObjects:
		var input pageInput
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return nil, err
		}
		return paginate(sortedDataObjects(workspace.resources.Snapshot.DataObjects), input), nil
	case ToolGetDataObjectSchema:
		var input struct {
			Key string `json:"key"`
		}
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return nil, err
		}
		for _, object := range workspace.resources.Snapshot.DataObjects {
			if object.Key == input.Key {
				return object, nil
			}
		}
		return nil, fmt.Errorf("el objeto %q no existe", input.Key)
	case ToolListContactFields:
		var input pageInput
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return nil, err
		}
		return paginate(workspace.resources.Snapshot.ContactFields, input), nil
	case ToolListConnectionsSafe:
		var input pageInput
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return nil, err
		}
		return paginate(workspace.resources.Connections, input), nil
	case ToolListWhatsAppTemplates:
		var input pageInput
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return nil, err
		}
		return paginate(workspace.resources.Snapshot.Templates, input), nil
	case ToolListVariables:
		var input pageInput
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return nil, err
		}
		return paginate(workspace.resources.Snapshot.Variables, input), nil
	case ToolInspectVariablesAtNode:
		var input struct {
			NodeID string `json:"nodeId"`
		}
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return nil, err
		}
		return authoring.InspectVariablesAtNode(workspace.candidate, input.NodeID, workspace.resources.Snapshot)
	case ToolListPlaybooks:
		return authoring.ListPlaybooks(), nil
	case ToolGetPlaybook:
		var input authoring.ArtifactRef
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return nil, err
		}
		bundle, exists := authoring.GetPlaybook(input.ID, input.Version)
		if !exists {
			return nil, fmt.Errorf("playbook %s@%s no existe", input.ID, input.Version)
		}
		return bundle, nil
	case ToolValidateCandidate:
		return map[string]any{
			"candidateChecksum": workspace.candidateChecksum,
			"report":            authoring.ValidateForAuthoring(workspace.candidate, workspace.resources.Snapshot),
		}, nil
	case ToolApplyFlowOperations:
		return workspace.applyOperations(call.Arguments)
	default:
		return nil, fmt.Errorf("function call %q no implementada", call.Name)
	}
}

func (workspace *turnWorkspace) getNodes(ids []string) ([]map[string]any, error) {
	document, err := authoring.ParseDocument(workspace.candidate)
	if err != nil {
		return nil, err
	}
	values, ok := document["nodes"].([]any)
	if !ok {
		return nil, fmt.Errorf("nodes inválido")
	}
	byID := make(map[string]map[string]any, len(values))
	for _, value := range values {
		node, _ := value.(map[string]any)
		id, _ := node["id"].(string)
		byID[id] = node
	}
	result := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		node, exists := byID[id]
		if !exists {
			return nil, fmt.Errorf("el nodo %q no existe", id)
		}
		result = append(result, node)
	}
	return result, nil
}

func (workspace *turnWorkspace) applyOperations(raw json.RawMessage) (any, error) {
	var input applyFlowOperationsInput
	if err := decodeArguments(raw, &input); err != nil {
		return nil, err
	}
	if len(input.Operations) == 0 {
		return nil, fmt.Errorf("operations no puede estar vacío")
	}
	if len(workspace.operations)+len(input.Operations) > workspace.maxOperations {
		return nil, &functionExecutionError{Code: "operation_budget_exceeded", Message: fmt.Sprintf("el turno supera %d operaciones", workspace.maxOperations)}
	}
	result, err := authoring.ApplyFlowOperations(workspace.candidate, input.Operations, authoring.ApplyOptions{
		ExpectedCandidateChecksum: input.ExpectedCandidateChecksum,
	})
	if err != nil {
		return nil, &functionExecutionError{Code: "invalid_operations", Message: err.Error()}
	}
	report := authoring.ValidateForAuthoring(result.Candidate, workspace.resources.Snapshot)
	workspace.candidate = append(json.RawMessage(nil), result.Candidate...)
	workspace.candidateChecksum = result.CandidateChecksum
	workspace.operations = append(workspace.operations, input.Operations...)
	return applyFlowOperationsOutput{
		CandidateChecksum: result.CandidateChecksum, AliasToNodeID: result.AliasToNodeID,
		AliasToEdgeID: result.AliasToEdgeID, Diff: result.Diff, Diagnostics: report.Diagnostics,
	}, nil
}

func (workspace *turnWorkspace) submit(raw json.RawMessage) (SubmitProposalInput, *ProposalCandidate, error) {
	var terminal SubmitProposalInput
	if err := decodeArguments(raw, &terminal); err != nil {
		return terminal, nil, err
	}
	if strings.TrimSpace(terminal.Response) == "" {
		return terminal, nil, fmt.Errorf("response es obligatorio")
	}
	switch terminal.Mode {
	case TerminalQuestion, TerminalExplanation:
		return terminal, nil, nil
	case TerminalProposal:
	default:
		return terminal, nil, fmt.Errorf("mode %q inválido", terminal.Mode)
	}
	if terminal.ExpectedCandidateChecksum != workspace.candidateChecksum {
		return terminal, nil, fmt.Errorf("expectedCandidateChecksum no coincide con el workspace")
	}
	report := authoring.ValidateForAuthoring(workspace.candidate, workspace.resources.Snapshot)
	if report.HasErrors() {
		return terminal, nil, fmt.Errorf("el candidato conserva errores deterministas")
	}
	diff, err := authoring.SemanticDiff(workspace.base, workspace.candidate)
	if err != nil {
		return terminal, nil, err
	}
	if diff.Empty() {
		return terminal, nil, fmt.Errorf("una propuesta debe contener un cambio; usa explanation si no hay diff")
	}
	bundleHashes := make(map[string]string, len(terminal.Playbooks))
	playbooks := append([]authoring.ArtifactRef(nil), terminal.Playbooks...)
	sort.Slice(playbooks, func(i, j int) bool {
		if playbooks[i].ID == playbooks[j].ID {
			return playbooks[i].Version < playbooks[j].Version
		}
		return playbooks[i].ID < playbooks[j].ID
	})
	for _, reference := range playbooks {
		bundle, exists := authoring.GetPlaybook(reference.ID, reference.Version)
		if !exists {
			return terminal, nil, fmt.Errorf("playbook %s@%s no existe", reference.ID, reference.Version)
		}
		bundleHashes[reference.ID+"@"+reference.Version] = bundle.BundleHash
	}
	proposal := &ProposalCandidate{
		Candidate: append(json.RawMessage(nil), workspace.candidate...), BaseChecksum: workspace.baseChecksum,
		StartingCandidateChecksum: workspace.startingCandidateChecksum, CandidateChecksum: workspace.candidateChecksum,
		Diff: diff, Operations: append([]authoring.FlowOperation(nil), workspace.operations...), Diagnostics: report.Diagnostics,
		KnowledgeBundleHashes: bundleHashes, PlaybookVersions: playbooks,
		CatalogHash: authoring.CatalogHash(), ResourceHash: workspace.resources.Hash,
	}
	return terminal, proposal, nil
}

func functionCallSignature(call FunctionCall) (string, error) {
	canonical, err := engine.Canonical(call.Arguments)
	if err != nil {
		return "", fmt.Errorf("argumentos de %s: %w", call.Name, err)
	}
	return call.Name + "\x00" + string(canonical), nil
}

func successfulFunctionResult(callID, name string, output any, maximum int) (FunctionResult, error) {
	raw, err := json.Marshal(output)
	if err != nil {
		return FunctionResult{}, err
	}
	if len(raw) > maximum {
		return FunctionResult{}, fmt.Errorf("el resultado de %s supera %d bytes; pagina o pide un recurso por id", name, maximum)
	}
	return FunctionResult{CallID: callID, Name: name, Output: raw}, nil
}

func errorFunctionResult(callID, name, code, message string, details any) FunctionResult {
	payload := map[string]any{"code": code, "message": message}
	if details != nil {
		payload["details"] = details
	}
	raw, _ := json.Marshal(payload)
	return FunctionResult{CallID: callID, Name: name, Output: raw, IsError: true}
}

func accumulateUsage(total *TurnUsage, usage ModelUsage) {
	total.Requests++
	total.InputTokens += usage.InputTokens
	total.OutputTokens += usage.OutputTokens
	total.CacheReadInputTokens += usage.CacheReadInputTokens
	total.CacheWriteInputTokens += usage.CacheWriteInputTokens
	total.CostUSD += usage.CostUSD
}

func boundedText(value string, maximum int) string {
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return string(runes[:maximum])
}

func boundedConversation(entries []ConversationEntry) []ConversationEntry {
	if len(entries) > 12 {
		entries = entries[len(entries)-12:]
	}
	result := make([]ConversationEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Role != "user" && entry.Role != "assistant" {
			continue
		}
		result = append(result, ConversationEntry{Role: entry.Role, Content: boundedText(entry.Content, 2000)})
	}
	return result
}
