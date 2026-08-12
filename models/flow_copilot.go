package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yzx7/sacs-chatbots/engine"
)

const (
	flowCopilotMaxDraftBytes      = 1 << 20
	flowCopilotMaxStructuredBytes = 256 << 10
	flowCopilotTurnTimeout        = 2 * time.Minute
	flowCopilotMaxTurnsPerSession = int64(50)
)

// FlowCopilotCapability es la capacidad server-side que acompaña el detalle del
// flujo. La autorización real se vuelve a comprobar en cada endpoint.
type FlowCopilotCapability struct {
	Enabled             bool   `json:"enabled"`
	Reason              string `json:"reason,omitempty"`
	ProviderOperational bool   `json:"providerOperational"`
	RemainingTurns      *int   `json:"remainingTurns,omitempty"`
}

// FlowCopilotSession y los DTO siguientes son neutrales: models no conoce al
// proveedor, prompts ni operaciones de authoring.
type FlowCopilotSession struct {
	ID             string     `db:"id" json:"id"`
	OrganizationID string     `db:"organization_id" json:"organizationId"`
	BotID          string     `db:"bot_id" json:"botId"`
	FlowID         string     `db:"flow_id" json:"flowId"`
	CreatedBy      string     `db:"created_by" json:"createdBy"`
	Title          string     `db:"title" json:"title"`
	Status         string     `db:"status" json:"status"`
	Summary        string     `db:"summary" json:"summary"`
	NextSequence   int64      `db:"next_sequence" json:"nextSequence"`
	CreatedAt      time.Time  `db:"created_at" json:"createdAt"`
	UpdatedAt      time.Time  `db:"updated_at" json:"updatedAt"`
	ClosedAt       *time.Time `db:"closed_at" json:"closedAt,omitempty"`
}

type FlowCopilotTurn struct {
	ID                     string          `db:"id" json:"id"`
	SessionID              string          `db:"session_id" json:"sessionId"`
	OrganizationID         string          `db:"organization_id" json:"organizationId"`
	CreatedBy              string          `db:"created_by" json:"createdBy"`
	Sequence               int64           `db:"sequence" json:"sequence"`
	UserMessage            string          `db:"user_message" json:"userMessage"`
	AssistantMessage       *string         `db:"assistant_message" json:"assistantMessage,omitempty"`
	Status                 string          `db:"status" json:"status"`
	Mode                   *string         `db:"mode" json:"mode,omitempty"`
	EditorRevision         string          `db:"editor_revision" json:"editorRevision"`
	PersistedDraftChecksum string          `db:"persisted_draft_checksum" json:"persistedDraftChecksum"`
	WorkingDraftChecksum   string          `db:"working_draft_checksum" json:"workingDraftChecksum"`
	ToolTrace              json.RawMessage `db:"tool_trace" json:"toolTrace"`
	PlaybookVersions       json.RawMessage `db:"playbook_versions" json:"playbookVersions"`
	CapabilityHash         *string         `db:"capability_hash" json:"capabilityHash,omitempty"`
	ResourceHash           *string         `db:"resource_hash" json:"resourceHash,omitempty"`
	KnowledgeBundleHash    *string         `db:"knowledge_bundle_hash" json:"knowledgeBundleHash,omitempty"`
	ErrorCode              *string         `db:"error_code" json:"errorCode,omitempty"`
	CreatedAt              time.Time       `db:"created_at" json:"createdAt"`
	CompletedAt            *time.Time      `db:"completed_at" json:"completedAt,omitempty"`
}

type FlowCopilotProposal struct {
	ID                    string          `db:"id" json:"id"`
	TurnID                string          `db:"turn_id" json:"turnId"`
	SessionID             string          `db:"session_id" json:"sessionId"`
	PersistedBase         json.RawMessage `db:"persisted_base" json:"persistedBase"`
	PersistedBaseChecksum string          `db:"persisted_base_checksum" json:"persistedBaseChecksum"`
	WorkingBase           json.RawMessage `db:"working_base" json:"workingBase"`
	WorkingBaseChecksum   string          `db:"working_base_checksum" json:"workingBaseChecksum"`
	EditorRevision        string          `db:"editor_revision" json:"editorRevision"`
	Candidate             json.RawMessage `db:"candidate" json:"candidate"`
	CandidateChecksum     string          `db:"candidate_checksum" json:"candidateChecksum"`
	Operations            json.RawMessage `db:"operations" json:"operations"`
	Diff                  json.RawMessage `db:"diff" json:"diff"`
	Assumptions           json.RawMessage `db:"assumptions" json:"assumptions"`
	Requirements          json.RawMessage `db:"requirements" json:"requirements"`
	Diagnostics           json.RawMessage `db:"diagnostics" json:"diagnostics"`
	PlaybookVersions      json.RawMessage `db:"playbook_versions" json:"playbookVersions"`
	KnowledgeBundleHash   *string         `db:"knowledge_bundle_hash" json:"knowledgeBundleHash,omitempty"`
	Status                string          `db:"status" json:"status"`
	AppliedBy             *string         `db:"applied_by" json:"appliedBy,omitempty"`
	AppliedAt             *time.Time      `db:"applied_at" json:"appliedAt,omitempty"`
	DismissedBy           *string         `db:"dismissed_by" json:"dismissedBy,omitempty"`
	DismissedAt           *time.Time      `db:"dismissed_at" json:"dismissedAt,omitempty"`
	UndoneBy              *string         `db:"undone_by" json:"undoneBy,omitempty"`
	UndoneAt              *time.Time      `db:"undone_at" json:"undoneAt,omitempty"`
	CreatedAt             time.Time       `db:"created_at" json:"createdAt"`
}

type FlowCopilotSessionDetail struct {
	Session   *FlowCopilotSession   `json:"session"`
	Turns     []FlowCopilotTurn     `json:"turns"`
	Proposals []FlowCopilotProposal `json:"proposals"`
}

type FlowCopilotConflictError struct {
	Code                   string `json:"code"`
	Problem                string `json:"-"`
	ProposalStatus         string `json:"proposalStatus,omitempty"`
	ExpectedEditorRevision string `json:"expectedEditorRevision,omitempty"`
	CurrentEditorRevision  string `json:"currentEditorRevision,omitempty"`
	ExpectedChecksum       string `json:"expectedChecksum,omitempty"`
	CurrentChecksum        string `json:"currentChecksum,omitempty"`
}

func (e *FlowCopilotConflictError) Error() string {
	if e.Problem != "" {
		return e.Problem
	}
	return "el estado del Copilot cambió"
}

type FlowCopilotInputError struct{ Problem string }

func (e *FlowCopilotInputError) Error() string { return e.Problem }

const flowCopilotSessionCols = `id::text AS id, organization_id::text AS organization_id,
	bot_id::text AS bot_id, flow_id::text AS flow_id, created_by, title, status, summary,
	next_sequence, created_at, updated_at, closed_at`

const flowCopilotTurnCols = `id::text AS id, session_id::text AS session_id,
	organization_id::text AS organization_id, created_by, sequence, user_message,
	assistant_message, status, mode, editor_revision, persisted_draft_checksum,
	working_draft_checksum, tool_trace, playbook_versions, capability_hash, resource_hash,
	knowledge_bundle_hash, error_code, created_at, completed_at`

const flowCopilotProposalCols = `id::text AS id, turn_id::text AS turn_id,
	session_id::text AS session_id, persisted_base, persisted_base_checksum, working_base,
	working_base_checksum, editor_revision, candidate, candidate_checksum, operations, diff,
	assumptions, requirements, diagnostics, playbook_versions, knowledge_bundle_hash, status,
	applied_by, applied_at, dismissed_by, dismissed_at, undone_by, undone_at, created_at`

type CreateFlowCopilotSessionParams struct {
	OrganizationID         string
	BotID                  string
	FlowID                 string
	CreatedBy              string
	Title                  string
	PersistedDraftChecksum string
	WorkingDraft           json.RawMessage
	EditorRevision         string
}

// CreateFlowCopilotSession comprueba dentro de la misma transacción que la
// revisión persistida aún existe. La copia de trabajo solo se verifica; cada
// turno vuelve a enviarla porque puede cambiar en el editor.
func CreateFlowCopilotSession(
	ctx context.Context,
	p *pgxpool.Pool,
	in CreateFlowCopilotSessionParams,
) (*FlowCopilotSession, error) {
	if strings.TrimSpace(in.CreatedBy) == "" || strings.TrimSpace(in.EditorRevision) == "" ||
		strings.TrimSpace(in.PersistedDraftChecksum) == "" {
		return nil, &FlowCopilotInputError{Problem: "createdBy, editorRevision y persistedDraftChecksum son obligatorios"}
	}
	if _, _, err := canonicalCopilotDocument(in.WorkingDraft); err != nil {
		return nil, err
	}
	if len(in.Title) > 200 {
		return nil, &FlowCopilotInputError{Problem: "el título excede 200 caracteres"}
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	flow, err := lockFlow(ctx, tx, in.BotID, in.FlowID)
	if err != nil || flow == nil {
		return nil, err
	}
	if flow.ArchivedAt != nil {
		return nil, ErrFlowArchived
	}
	var owns bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM bots WHERE id=$1::uuid AND org_id=$2::uuid)`,
		in.BotID, in.OrganizationID).Scan(&owns); err != nil {
		return nil, err
	}
	if !owns {
		return nil, nil
	}
	current, err := DraftSnapshotFromFlow(flow)
	if err != nil {
		return nil, err
	}
	if current.Checksum != strings.TrimSpace(in.PersistedDraftChecksum) {
		return nil, newDraftConflict(strings.TrimSpace(in.PersistedDraftChecksum), current)
	}

	rows, err := tx.Query(ctx,
		`INSERT INTO flow_copilot_sessions
		 (organization_id, bot_id, flow_id, created_by, title)
		 VALUES ($1::uuid,$2::uuid,$3::uuid,$4,$5)
		 RETURNING `+flowCopilotSessionCols,
		in.OrganizationID, in.BotID, in.FlowID, in.CreatedBy, strings.TrimSpace(in.Title))
	if err != nil {
		return nil, err
	}
	session, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowCopilotSession])
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &session, nil
}

func ListFlowCopilotSessions(
	ctx context.Context,
	p *pgxpool.Pool,
	organizationID, botID, flowID, createdBy, status string,
) ([]FlowCopilotSession, error) {
	if status != "" && status != "active" && status != "closed" {
		return nil, &FlowCopilotInputError{Problem: "status debe ser active o closed"}
	}
	rows, err := p.Query(ctx,
		`SELECT `+flowCopilotSessionCols+` FROM flow_copilot_sessions
		 WHERE organization_id=$1::uuid AND bot_id=$2::uuid AND flow_id=$3::uuid
		   AND created_by=$4 AND ($5='' OR status=$5)
		 ORDER BY updated_at DESC LIMIT 50`,
		organizationID, botID, flowID, createdBy, status)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[FlowCopilotSession])
}

func GetFlowCopilotSession(
	ctx context.Context,
	p *pgxpool.Pool,
	organizationID, botID, flowID, sessionID, createdBy string,
) (*FlowCopilotSessionDetail, error) {
	rows, err := p.Query(ctx,
		`SELECT `+flowCopilotSessionCols+` FROM flow_copilot_sessions
		 WHERE id=$1::uuid AND organization_id=$2::uuid AND bot_id=$3::uuid
		   AND flow_id=$4::uuid AND created_by=$5`,
		sessionID, organizationID, botID, flowID, createdBy)
	if err != nil {
		return nil, err
	}
	session, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowCopilotSession])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	turnRows, err := p.Query(ctx,
		`SELECT `+flowCopilotTurnCols+` FROM flow_copilot_turns
		 WHERE session_id=$1::uuid ORDER BY sequence`, sessionID)
	if err != nil {
		return nil, err
	}
	turns, err := pgx.CollectRows(turnRows, pgx.RowToStructByName[FlowCopilotTurn])
	if err != nil {
		return nil, err
	}
	proposalRows, err := p.Query(ctx,
		`SELECT `+flowCopilotProposalCols+` FROM flow_copilot_proposals
		 WHERE session_id=$1::uuid ORDER BY created_at`, sessionID)
	if err != nil {
		return nil, err
	}
	proposals, err := pgx.CollectRows(proposalRows, pgx.RowToStructByName[FlowCopilotProposal])
	if err != nil {
		return nil, err
	}
	return &FlowCopilotSessionDetail{Session: &session, Turns: turns, Proposals: proposals}, nil
}

type CreateFlowCopilotTurnParams struct {
	OrganizationID            string
	BotID                     string
	FlowID                    string
	SessionID                 string
	CreatedBy                 string
	Message                   string
	PersistedDraftChecksum    string
	WorkingDraft              json.RawMessage
	EditorRevision            string
	ReviseProposalID          string
	ExpectedCandidateChecksum string
	StaleAfter                time.Duration
}

// CreateFlowCopilotTurn reserva una secuencia sin mantener locks durante la
// llamada al proveedor. También recupera turnos huérfanos antes de que el índice
// único parcial bloquee al usuario indefinidamente.
func CreateFlowCopilotTurn(
	ctx context.Context,
	p *pgxpool.Pool,
	in CreateFlowCopilotTurnParams,
) (*FlowCopilotTurn, error) {
	in.Message = strings.TrimSpace(in.Message)
	in.EditorRevision = strings.TrimSpace(in.EditorRevision)
	in.PersistedDraftChecksum = strings.TrimSpace(in.PersistedDraftChecksum)
	if len(in.Message) < 1 || len(in.Message) > 8000 {
		return nil, &FlowCopilotInputError{Problem: "message debe tener entre 1 y 8000 caracteres"}
	}
	if in.EditorRevision == "" || in.PersistedDraftChecksum == "" || strings.TrimSpace(in.CreatedBy) == "" {
		return nil, &FlowCopilotInputError{Problem: "editorRevision y persistedDraftChecksum son obligatorios"}
	}
	_, workingChecksum, err := canonicalCopilotDocument(in.WorkingDraft)
	if err != nil {
		return nil, err
	}
	if (in.ReviseProposalID == "") != (in.ExpectedCandidateChecksum == "") {
		return nil, &FlowCopilotInputError{Problem: "expectedCandidateChecksum es obligatorio al revisar una propuesta"}
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	flow, err := lockFlow(ctx, tx, in.BotID, in.FlowID)
	if err != nil || flow == nil {
		return nil, err
	}
	if flow.ArchivedAt != nil {
		return nil, ErrFlowArchived
	}
	current, err := DraftSnapshotFromFlow(flow)
	if err != nil {
		return nil, err
	}
	if current.Checksum != in.PersistedDraftChecksum {
		return nil, newDraftConflict(in.PersistedDraftChecksum, current)
	}

	session, err := lockFlowCopilotSession(ctx, tx, in.OrganizationID, in.BotID, in.FlowID, in.SessionID, in.CreatedBy)
	if err != nil || session == nil {
		return nil, err
	}
	if session.Status != "active" {
		return nil, copilotConflict("session_closed", "la sesión del Copilot está cerrada")
	}
	if session.NextSequence > flowCopilotMaxTurnsPerSession {
		return nil, copilotConflict("session_turn_limit", "la sesión alcanzó el límite de 50 turnos")
	}
	if in.ReviseProposalID != "" {
		proposal, err := lockFlowCopilotProposal(ctx, tx, in.OrganizationID, in.BotID, in.FlowID,
			in.SessionID, in.ReviseProposalID, in.CreatedBy)
		if err != nil {
			return nil, err
		}
		if proposal == nil || proposal.Status != "pending" || proposal.CandidateChecksum != in.ExpectedCandidateChecksum {
			conflict := copilotConflict("revise_proposal_unavailable", "la propuesta que se quería revisar ya no está vigente")
			if proposal != nil {
				conflict.ProposalStatus = proposal.Status
				conflict.ExpectedChecksum = in.ExpectedCandidateChecksum
				conflict.CurrentChecksum = proposal.CandidateChecksum
			}
			return nil, conflict
		}
		if proposal.WorkingBaseChecksum != workingChecksum || proposal.EditorRevision != in.EditorRevision {
			conflict := copilotConflict("proposal_rebase_required", "el lienzo cambió; primero hay que rebasar la propuesta")
			conflict.ExpectedChecksum = proposal.WorkingBaseChecksum
			conflict.CurrentChecksum = workingChecksum
			conflict.ExpectedEditorRevision = proposal.EditorRevision
			conflict.CurrentEditorRevision = in.EditorRevision
			return nil, conflict
		}
	}

	staleAfter := in.StaleAfter
	if staleAfter <= 0 {
		staleAfter = flowCopilotTurnTimeout
	}
	staleSeconds := int64(staleAfter / time.Second)
	if staleSeconds < 1 {
		staleSeconds = 1
	}
	if _, err := tx.Exec(ctx,
		`UPDATE flow_copilot_turns
		 SET status='failed', error_code='timeout', completed_at=NOW()
		 WHERE organization_id=$1::uuid AND created_by=$2 AND status='running'
		   AND created_at < NOW() - ($3 * INTERVAL '1 second')`,
		in.OrganizationID, in.CreatedBy, staleSeconds); err != nil {
		return nil, err
	}
	var sequence int64
	if err := tx.QueryRow(ctx,
		`UPDATE flow_copilot_sessions SET next_sequence=next_sequence+1
		 WHERE id=$1::uuid RETURNING next_sequence-1`, session.ID).Scan(&sequence); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx,
		`INSERT INTO flow_copilot_turns
		 (session_id, organization_id, created_by, sequence, user_message,
		  editor_revision, persisted_draft_checksum, working_draft_checksum)
		 VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8)
		 RETURNING `+flowCopilotTurnCols,
		session.ID, in.OrganizationID, in.CreatedBy, sequence, in.Message,
		in.EditorRevision, in.PersistedDraftChecksum, workingChecksum)
	if err != nil {
		return nil, translateFlowCopilotConstraint(err)
	}
	turn, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowCopilotTurn])
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, translateFlowCopilotConstraint(err)
	}
	return &turn, nil
}

type NewFlowCopilotProposal struct {
	PersistedBase       json.RawMessage
	WorkingBase         json.RawMessage
	EditorRevision      string
	Candidate           json.RawMessage
	Operations          json.RawMessage
	Diff                json.RawMessage
	Assumptions         json.RawMessage
	Requirements        json.RawMessage
	Diagnostics         json.RawMessage
	PlaybookVersions    json.RawMessage
	KnowledgeBundleHash string
}

// FlowCopilotCandidateValidator permite que la capa copilot/authoring valide
// bindings sin invertir dependencias: models no importa esos paquetes. Se
// ejecuta con la misma pgx.Tx que completa o aplica la propuesta.
type FlowCopilotCandidateValidator func(
	context.Context,
	pgx.Tx,
	string,
	string,
	json.RawMessage,
) error

type CompleteFlowCopilotTurnParams struct {
	OrganizationID      string
	BotID               string
	FlowID              string
	SessionID           string
	TurnID              string
	CreatedBy           string
	AssistantMessage    string
	Mode                string
	ToolTrace           json.RawMessage
	PlaybookVersions    json.RawMessage
	CapabilityHash      string
	ResourceHash        string
	KnowledgeBundleHash string
	Proposal            *NewFlowCopilotProposal
	ValidateCandidate   FlowCopilotCandidateValidator
}

type CompleteFlowCopilotTurnResult struct {
	Turn     *FlowCopilotTurn     `json:"turn"`
	Proposal *FlowCopilotProposal `json:"proposal,omitempty"`
}

// CompleteFlowCopilotTurn persiste respuesta y propuesta en una sola
// transacción. Una revisión vuelve stale la propuesta previa solo cuando existe
// una nueva propuesta completa; un fallo del proveedor no destruye la anterior.
func CompleteFlowCopilotTurn(
	ctx context.Context,
	p *pgxpool.Pool,
	in CompleteFlowCopilotTurnParams,
) (*CompleteFlowCopilotTurnResult, error) {
	if in.Mode != "question" && in.Mode != "explanation" && in.Mode != "proposal" {
		return nil, &FlowCopilotInputError{Problem: "mode debe ser question, explanation o proposal"}
	}
	if in.Mode == "proposal" && in.Proposal == nil {
		return nil, &FlowCopilotInputError{Problem: "un turno proposal requiere propuesta"}
	}
	if in.Mode != "proposal" && in.Proposal != nil {
		return nil, &FlowCopilotInputError{Problem: "solo un turno proposal puede adjuntar propuesta"}
	}
	toolTrace, err := normalizeCopilotToolTrace(in.ToolTrace)
	if err != nil {
		return nil, err
	}
	turnPlaybooks, err := normalizeCopilotJSON(in.PlaybookVersions, []byte(`[]`), '[', flowCopilotMaxStructuredBytes, "playbookVersions")
	if err != nil {
		return nil, err
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var completionDraft *DraftSnapshot
	var completionFlow *Flow
	if in.Proposal != nil {
		if in.ValidateCandidate == nil {
			return nil, &FlowCopilotInputError{Problem: "falta el validador de bindings del candidato"}
		}
		flow, err := lockFlow(ctx, tx, in.BotID, in.FlowID)
		if err != nil || flow == nil {
			return nil, err
		}
		if flow.ArchivedAt != nil {
			return nil, ErrFlowArchived
		}
		var owns bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM bots WHERE id=$1::uuid AND org_id=$2::uuid)`,
			in.BotID, in.OrganizationID).Scan(&owns); err != nil {
			return nil, err
		}
		if !owns {
			return nil, nil
		}
		completionFlow = flow
		completionDraft, err = DraftSnapshotFromFlow(flow)
		if err != nil {
			return nil, err
		}
	}
	session, err := lockFlowCopilotSession(ctx, tx, in.OrganizationID, in.BotID, in.FlowID, in.SessionID, in.CreatedBy)
	if err != nil || session == nil {
		return nil, err
	}
	turn, err := lockFlowCopilotTurn(ctx, tx, session.ID, in.TurnID, in.CreatedBy)
	if err != nil || turn == nil {
		return nil, err
	}
	if turn.Status == "completed" {
		proposal, err := getFlowCopilotProposalByTurnTx(ctx, tx, turn.ID)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &CompleteFlowCopilotTurnResult{Turn: turn, Proposal: proposal}, nil
	}
	if turn.Status != "running" {
		conflict := copilotConflict("turn_not_running", "el turno ya no está en ejecución")
		conflict.ProposalStatus = turn.Status
		return nil, conflict
	}
	if in.Proposal != nil {
		candidateFlow := *completionFlow
		candidateFlow.Draft = in.Proposal.Candidate
		if _, _, _, err := validateLockedFlowForPublish(ctx, tx, in.BotID, &candidateFlow); err != nil {
			return nil, err
		}
		if err := in.ValidateCandidate(ctx, tx, in.OrganizationID, in.BotID, in.Proposal.Candidate); err != nil {
			return nil, err
		}
	}

	var proposal *FlowCopilotProposal
	if in.Proposal != nil {
		proposalStatus := "pending"
		var rebasedPersisted *DraftSnapshot
		if completionDraft.Checksum != turn.PersistedDraftChecksum {
			if completionDraft.Checksum == turn.WorkingDraftChecksum {
				rebasedPersisted = completionDraft
			} else {
				proposalStatus = "stale"
			}
		}
		proposal, err = createFlowCopilotProposalTx(ctx, tx, session, turn, *in.Proposal,
			proposalStatus, rebasedPersisted)
		if err != nil {
			return nil, err
		}
	}
	mode := in.Mode
	assistant := in.AssistantMessage
	rows, err := tx.Query(ctx,
		`UPDATE flow_copilot_turns
		 SET assistant_message=$2, status='completed', mode=$3, tool_trace=$4,
		     playbook_versions=$5, capability_hash=NULLIF($6,''), resource_hash=NULLIF($7,''),
		     knowledge_bundle_hash=NULLIF($8,''), error_code=NULL, completed_at=NOW()
		 WHERE id=$1::uuid RETURNING `+flowCopilotTurnCols,
		turn.ID, assistant, mode, toolTrace, turnPlaybooks, in.CapabilityHash, in.ResourceHash, in.KnowledgeBundleHash)
	if err != nil {
		return nil, err
	}
	completed, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowCopilotTurn])
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &CompleteFlowCopilotTurnResult{Turn: &completed, Proposal: proposal}, nil
}

func createFlowCopilotProposalTx(
	ctx context.Context,
	tx pgx.Tx,
	session *FlowCopilotSession,
	turn *FlowCopilotTurn,
	in NewFlowCopilotProposal,
	status string,
	rebasedPersisted *DraftSnapshot,
) (*FlowCopilotProposal, error) {
	persisted, persistedChecksum, err := canonicalCopilotDocument(in.PersistedBase)
	if err != nil {
		return nil, err
	}
	working, workingChecksum, err := canonicalCopilotDocument(in.WorkingBase)
	if err != nil {
		return nil, err
	}
	candidate, candidateChecksum, err := canonicalCopilotDocument(in.Candidate)
	if err != nil {
		return nil, err
	}
	if persistedChecksum != turn.PersistedDraftChecksum || workingChecksum != turn.WorkingDraftChecksum ||
		strings.TrimSpace(in.EditorRevision) != turn.EditorRevision {
		return nil, copilotConflict("proposal_provenance_mismatch", "la propuesta no corresponde al contexto con que inició el turno")
	}
	if rebasedPersisted != nil {
		persisted = rebasedPersisted.Draft
		persistedChecksum = rebasedPersisted.Checksum
	}
	if status != "pending" && status != "stale" {
		return nil, &FlowCopilotInputError{Problem: "estado inicial de propuesta inválido"}
	}
	operations, err := normalizeCopilotJSON(in.Operations, []byte(`[]`), '[', flowCopilotMaxStructuredBytes, "operations")
	if err != nil {
		return nil, err
	}
	diff, err := normalizeCopilotJSON(in.Diff, []byte(`{}`), '{', flowCopilotMaxStructuredBytes, "diff")
	if err != nil {
		return nil, err
	}
	assumptions, err := normalizeCopilotJSON(in.Assumptions, []byte(`[]`), '[', flowCopilotMaxStructuredBytes, "assumptions")
	if err != nil {
		return nil, err
	}
	requirements, err := normalizeCopilotJSON(in.Requirements, []byte(`[]`), '[', flowCopilotMaxStructuredBytes, "requirements")
	if err != nil {
		return nil, err
	}
	diagnostics, err := normalizeCopilotJSON(in.Diagnostics, []byte(`[]`), '[', flowCopilotMaxStructuredBytes, "diagnostics")
	if err != nil {
		return nil, err
	}
	playbooks, err := normalizeCopilotJSON(in.PlaybookVersions, []byte(`[]`), '[', flowCopilotMaxStructuredBytes, "playbookVersions")
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE flow_copilot_proposals SET status='stale'
		 WHERE session_id=$1::uuid AND status='pending'`, session.ID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx,
		`INSERT INTO flow_copilot_proposals
		 (turn_id, session_id, persisted_base, persisted_base_checksum, working_base,
		  working_base_checksum, editor_revision, candidate, candidate_checksum,
		  operations, diff, assumptions, requirements, diagnostics, playbook_versions,
		  knowledge_bundle_hash, status)
		 VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,NULLIF($16,''),$17)
		 RETURNING `+flowCopilotProposalCols,
		turn.ID, session.ID, persisted, persistedChecksum, working, workingChecksum,
		turn.EditorRevision, candidate, candidateChecksum, operations, diff, assumptions,
		requirements, diagnostics, playbooks, in.KnowledgeBundleHash, status)
	if err != nil {
		return nil, translateFlowCopilotConstraint(err)
	}
	proposal, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowCopilotProposal])
	if err != nil {
		return nil, err
	}
	return &proposal, nil
}

func FailFlowCopilotTurn(
	ctx context.Context,
	p *pgxpool.Pool,
	organizationID, botID, flowID, sessionID, turnID, createdBy, errorCode string,
) (*FlowCopilotTurn, error) {
	return finishFlowCopilotTurn(ctx, p, organizationID, botID, flowID, sessionID, turnID,
		createdBy, "failed", strings.TrimSpace(errorCode))
}

func CancelFlowCopilotTurn(
	ctx context.Context,
	p *pgxpool.Pool,
	organizationID, botID, flowID, turnID, createdBy string,
) (*FlowCopilotTurn, error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx,
		`SELECT `+flowCopilotTurnCols+` FROM flow_copilot_turns
		 WHERE id=$1::uuid AND created_by=$5
		   AND session_id=(SELECT id FROM flow_copilot_sessions
		   WHERE id=flow_copilot_turns.session_id AND organization_id=$2::uuid
		     AND bot_id=$3::uuid AND flow_id=$4::uuid AND created_by=$5)
		 FOR UPDATE`,
		turnID, organizationID, botID, flowID, createdBy)
	if err != nil {
		return nil, err
	}
	turn, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowCopilotTurn])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if turn.Status == "running" {
		updatedRows, err := tx.Query(ctx,
			`UPDATE flow_copilot_turns SET status='cancelled', error_code='cancelled', completed_at=NOW()
			 WHERE id=$1::uuid RETURNING `+flowCopilotTurnCols, turn.ID)
		if err != nil {
			return nil, err
		}
		updated, err := pgx.CollectExactlyOneRow(updatedRows, pgx.RowToStructByName[FlowCopilotTurn])
		if err != nil {
			return nil, err
		}
		turn = updated
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &turn, nil
}

func finishFlowCopilotTurn(
	ctx context.Context,
	p *pgxpool.Pool,
	organizationID, botID, flowID, sessionID, turnID, createdBy, status, errorCode string,
) (*FlowCopilotTurn, error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	session, err := lockFlowCopilotSession(ctx, tx, organizationID, botID, flowID, sessionID, createdBy)
	if err != nil || session == nil {
		return nil, err
	}
	turn, err := lockFlowCopilotTurn(ctx, tx, session.ID, turnID, createdBy)
	if err != nil || turn == nil {
		return nil, err
	}
	if turn.Status == "running" {
		rows, err := tx.Query(ctx,
			`UPDATE flow_copilot_turns SET status=$2, error_code=NULLIF($3,''), completed_at=NOW()
			 WHERE id=$1::uuid RETURNING `+flowCopilotTurnCols, turn.ID, status, errorCode)
		if err != nil {
			return nil, err
		}
		updated, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowCopilotTurn])
		if err != nil {
			return nil, err
		}
		turn = &updated
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return turn, nil
}

func CloseFlowCopilotSession(
	ctx context.Context,
	p *pgxpool.Pool,
	organizationID, botID, flowID, sessionID, createdBy string,
) (*FlowCopilotSession, error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	session, err := lockFlowCopilotSession(ctx, tx, organizationID, botID, flowID, sessionID, createdBy)
	if err != nil || session == nil {
		return nil, err
	}
	if session.Status == "active" {
		if _, err := tx.Exec(ctx,
			`UPDATE flow_copilot_turns SET status='cancelled', error_code='session_closed', completed_at=NOW()
			 WHERE session_id=$1::uuid AND status='running'`, session.ID); err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx,
			`UPDATE flow_copilot_sessions SET status='closed', closed_at=NOW()
			 WHERE id=$1::uuid RETURNING `+flowCopilotSessionCols, session.ID)
		if err != nil {
			return nil, err
		}
		closed, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowCopilotSession])
		if err != nil {
			return nil, err
		}
		session = &closed
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return session, nil
}

type ApplyFlowCopilotProposalParams struct {
	OrganizationID         string
	BotID                  string
	FlowID                 string
	ProposalID             string
	CreatedBy              string
	ExpectedDraftChecksum  string
	ExpectedEditorRevision string
	CurrentWorkingDraft    json.RawMessage
	ValidateCandidate      FlowCopilotCandidateValidator
}

type FlowCopilotDraftMutationResult struct {
	DraftSnapshot *DraftSnapshot       `json:"draftSnapshot"`
	Proposal      *FlowCopilotProposal `json:"proposal"`
	Warnings      []string             `json:"warnings,omitempty"`
}

// ApplyFlowCopilotProposal es el único puente propuesta→draft. Bloquea siempre
// flujo y luego propuesta, vuelve a validar el candidato y confirma ambos
// cambios en una sola transacción. Nunca publica.
func ApplyFlowCopilotProposal(
	ctx context.Context,
	p *pgxpool.Pool,
	in ApplyFlowCopilotProposalParams,
) (*FlowCopilotDraftMutationResult, error) {
	in.ExpectedDraftChecksum = strings.TrimSpace(in.ExpectedDraftChecksum)
	in.ExpectedEditorRevision = strings.TrimSpace(in.ExpectedEditorRevision)
	_, workingChecksum, err := canonicalCopilotDocument(in.CurrentWorkingDraft)
	if err != nil {
		return nil, err
	}
	if in.ExpectedDraftChecksum == "" || in.ExpectedEditorRevision == "" {
		return nil, &FlowCopilotInputError{Problem: "expectedDraftChecksum y expectedEditorRevision son obligatorios"}
	}
	if in.ValidateCandidate == nil {
		return nil, &FlowCopilotInputError{Problem: "falta el validador de bindings del candidato"}
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	flow, err := lockFlow(ctx, tx, in.BotID, in.FlowID)
	if err != nil || flow == nil {
		return nil, err
	}
	if flow.ArchivedAt != nil {
		return nil, ErrFlowArchived
	}
	current, err := DraftSnapshotFromFlow(flow)
	if err != nil {
		return nil, err
	}
	proposal, err := lockFlowCopilotProposal(ctx, tx, in.OrganizationID, in.BotID, in.FlowID,
		"", in.ProposalID, in.CreatedBy)
	if err != nil || proposal == nil {
		return nil, err
	}
	if proposal.Status == "applied" {
		if current.Checksum != proposal.CandidateChecksum {
			return nil, newDraftConflict(proposal.CandidateChecksum, current)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &FlowCopilotDraftMutationResult{DraftSnapshot: current, Proposal: proposal}, nil
	}
	if proposal.Status != "pending" {
		conflict := copilotConflict("proposal_not_pending", "la propuesta ya no está pendiente")
		conflict.ProposalStatus = proposal.Status
		return nil, conflict
	}
	if in.ExpectedEditorRevision != proposal.EditorRevision {
		conflict := copilotConflict("editor_revision_mismatch", "el lienzo cambió desde que se preparó la propuesta")
		conflict.ExpectedEditorRevision = proposal.EditorRevision
		conflict.CurrentEditorRevision = in.ExpectedEditorRevision
		return nil, conflict
	}
	if workingChecksum != proposal.WorkingBaseChecksum {
		conflict := copilotConflict("working_draft_mismatch", "la copia visible ya no coincide con la base de la propuesta")
		conflict.ExpectedChecksum = proposal.WorkingBaseChecksum
		conflict.CurrentChecksum = workingChecksum
		return nil, conflict
	}
	if current.Checksum != in.ExpectedDraftChecksum {
		return nil, newDraftConflict(in.ExpectedDraftChecksum, current)
	}
	if in.ExpectedDraftChecksum != proposal.PersistedBaseChecksum {
		conflict := copilotConflict("proposal_base_mismatch", "la propuesta fue preparada sobre otra revisión persistida")
		conflict.ExpectedChecksum = proposal.PersistedBaseChecksum
		conflict.CurrentChecksum = in.ExpectedDraftChecksum
		return nil, conflict
	}

	candidateFlow := *flow
	candidateFlow.Draft = proposal.Candidate
	canonicalCandidate, candidateChecksum, warnings, err := validateLockedFlowForPublish(ctx, tx, in.BotID, &candidateFlow)
	if err != nil {
		return nil, err
	}
	if candidateChecksum != proposal.CandidateChecksum {
		return nil, fmt.Errorf("proposal %s: candidate checksum almacenado no coincide", proposal.ID)
	}
	if err := in.ValidateCandidate(ctx, tx, in.OrganizationID, in.BotID, canonicalCandidate); err != nil {
		return nil, err
	}
	snapshot, err := updateFlowDraftTx(ctx, tx, flow, canonicalCandidate, in.ExpectedDraftChecksum, in.CreatedBy)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx,
		`UPDATE flow_copilot_proposals
		 SET status='applied', applied_by=$2, applied_at=NOW()
		 WHERE id=$1::uuid RETURNING `+flowCopilotProposalCols,
		proposal.ID, in.CreatedBy)
	if err != nil {
		return nil, err
	}
	applied, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowCopilotProposal])
	if err != nil {
		return nil, err
	}
	if err := reconcileFlowCopilotProposalsAfterDraftUpdateTx(ctx, tx, flow.ID,
		snapshot.Draft, snapshot.Checksum, proposal.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &FlowCopilotDraftMutationResult{DraftSnapshot: snapshot, Proposal: &applied, Warnings: warnings}, nil
}

func DismissFlowCopilotProposal(
	ctx context.Context,
	p *pgxpool.Pool,
	organizationID, botID, flowID, proposalID, createdBy string,
) (*FlowCopilotProposal, error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	proposal, err := lockFlowCopilotProposal(ctx, tx, organizationID, botID, flowID, "", proposalID, createdBy)
	if err != nil || proposal == nil {
		return nil, err
	}
	if proposal.Status == "dismissed" {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return proposal, nil
	}
	if proposal.Status != "pending" {
		conflict := copilotConflict("proposal_not_pending", "solo una propuesta pendiente se puede descartar")
		conflict.ProposalStatus = proposal.Status
		return nil, conflict
	}
	rows, err := tx.Query(ctx,
		`UPDATE flow_copilot_proposals
		 SET status='dismissed', dismissed_by=$2, dismissed_at=NOW()
		 WHERE id=$1::uuid RETURNING `+flowCopilotProposalCols,
		proposal.ID, createdBy)
	if err != nil {
		return nil, err
	}
	dismissed, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowCopilotProposal])
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &dismissed, nil
}

type UndoFlowCopilotProposalParams struct {
	OrganizationID        string
	BotID                 string
	FlowID                string
	ProposalID            string
	CreatedBy             string
	ExpectedDraftChecksum string
}

// UndoFlowCopilotProposal restaura la copia de trabajo exacta previa. No exige
// engine.Validate: los borradores incompletos también son estados legítimos.
func UndoFlowCopilotProposal(
	ctx context.Context,
	p *pgxpool.Pool,
	in UndoFlowCopilotProposalParams,
) (*FlowCopilotDraftMutationResult, error) {
	in.ExpectedDraftChecksum = strings.TrimSpace(in.ExpectedDraftChecksum)
	if in.ExpectedDraftChecksum == "" {
		return nil, &FlowCopilotInputError{Problem: "expectedDraftChecksum es obligatorio"}
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	flow, err := lockFlow(ctx, tx, in.BotID, in.FlowID)
	if err != nil || flow == nil {
		return nil, err
	}
	if flow.ArchivedAt != nil {
		return nil, ErrFlowArchived
	}
	current, err := DraftSnapshotFromFlow(flow)
	if err != nil {
		return nil, err
	}
	proposal, err := lockFlowCopilotProposal(ctx, tx, in.OrganizationID, in.BotID, in.FlowID,
		"", in.ProposalID, in.CreatedBy)
	if err != nil || proposal == nil {
		return nil, err
	}
	if proposal.Status == "undone" {
		if current.Checksum != proposal.WorkingBaseChecksum {
			return nil, newDraftConflict(proposal.WorkingBaseChecksum, current)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &FlowCopilotDraftMutationResult{DraftSnapshot: current, Proposal: proposal}, nil
	}
	if proposal.Status != "applied" {
		conflict := copilotConflict("proposal_not_applied", "solo una propuesta aplicada se puede deshacer")
		conflict.ProposalStatus = proposal.Status
		return nil, conflict
	}
	if current.Checksum != in.ExpectedDraftChecksum {
		return nil, newDraftConflict(in.ExpectedDraftChecksum, current)
	}
	if current.Checksum != proposal.CandidateChecksum {
		conflict := copilotConflict("proposal_result_changed", "el borrador cambió después de aplicar la propuesta")
		conflict.ExpectedChecksum = proposal.CandidateChecksum
		conflict.CurrentChecksum = current.Checksum
		return nil, conflict
	}
	snapshot, err := updateFlowDraftTx(ctx, tx, flow, proposal.WorkingBase, current.Checksum, in.CreatedBy)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx,
		`UPDATE flow_copilot_proposals SET status='undone', undone_by=$2, undone_at=NOW()
		 WHERE id=$1::uuid RETURNING `+flowCopilotProposalCols,
		proposal.ID, in.CreatedBy)
	if err != nil {
		return nil, err
	}
	undone, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowCopilotProposal])
	if err != nil {
		return nil, err
	}
	if err := reconcileFlowCopilotProposalsAfterDraftUpdateTx(ctx, tx, flow.ID,
		snapshot.Draft, snapshot.Checksum, proposal.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &FlowCopilotDraftMutationResult{DraftSnapshot: snapshot, Proposal: &undone}, nil
}

func lockFlowCopilotSession(
	ctx context.Context,
	tx pgx.Tx,
	organizationID, botID, flowID, sessionID, createdBy string,
) (*FlowCopilotSession, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+flowCopilotSessionCols+` FROM flow_copilot_sessions
		 WHERE id=$1::uuid AND organization_id=$2::uuid AND bot_id=$3::uuid
		   AND flow_id=$4::uuid AND created_by=$5 FOR UPDATE`,
		sessionID, organizationID, botID, flowID, createdBy)
	if err != nil {
		return nil, err
	}
	session, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowCopilotSession])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &session, nil
}

func lockFlowCopilotTurn(
	ctx context.Context,
	tx pgx.Tx,
	sessionID, turnID, createdBy string,
) (*FlowCopilotTurn, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+flowCopilotTurnCols+` FROM flow_copilot_turns
		 WHERE id=$1::uuid AND session_id=$2::uuid AND created_by=$3 FOR UPDATE`,
		turnID, sessionID, createdBy)
	if err != nil {
		return nil, err
	}
	turn, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowCopilotTurn])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &turn, nil
}

func lockFlowCopilotProposal(
	ctx context.Context,
	tx pgx.Tx,
	organizationID, botID, flowID, sessionID, proposalID, createdBy string,
) (*FlowCopilotProposal, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+flowCopilotProposalCols+` FROM flow_copilot_proposals
		 WHERE id=$1::uuid
		   AND session_id=(SELECT id FROM flow_copilot_sessions
		      WHERE id=flow_copilot_proposals.session_id
		        AND organization_id=$2::uuid AND bot_id=$3::uuid AND flow_id=$4::uuid
		        AND created_by=$5
		        AND (NULLIF($6,'') IS NULL OR id=NULLIF($6,'')::uuid))
		 FOR UPDATE`,
		proposalID, organizationID, botID, flowID, createdBy, sessionID)
	if err != nil {
		return nil, err
	}
	proposal, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowCopilotProposal])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &proposal, nil
}

func getFlowCopilotProposalByTurnTx(
	ctx context.Context,
	tx pgx.Tx,
	turnID string,
) (*FlowCopilotProposal, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+flowCopilotProposalCols+` FROM flow_copilot_proposals WHERE turn_id=$1::uuid`, turnID)
	if err != nil {
		return nil, err
	}
	proposal, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FlowCopilotProposal])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &proposal, nil
}

// reconcileFlowCopilotProposalsAfterDraftUpdateTx mantiene viva una propuesta
// solo si el nuevo draft coincide exactamente con su working base. Cualquier
// otra edición la vuelve stale; nunca intenta un merge silencioso.
func reconcileFlowCopilotProposalsAfterDraftUpdateTx(
	ctx context.Context,
	tx pgx.Tx,
	flowID string,
	newDraft json.RawMessage,
	newChecksum, excludedProposalID string,
) error {
	_, err := tx.Exec(ctx,
		`UPDATE flow_copilot_proposals p
		 SET status=CASE WHEN p.working_base_checksum=$2 THEN 'pending' ELSE 'stale' END,
		     persisted_base=CASE WHEN p.working_base_checksum=$2 THEN $3::jsonb ELSE p.persisted_base END,
		     persisted_base_checksum=CASE WHEN p.working_base_checksum=$2 THEN $2 ELSE p.persisted_base_checksum END
		 FROM flow_copilot_sessions s
		 WHERE s.id=p.session_id AND s.flow_id=$1::uuid AND p.status='pending'
		   AND (NULLIF($4,'') IS NULL OR p.id<>NULLIF($4,'')::uuid)`,
		flowID, newChecksum, newDraft, excludedProposalID)
	return err
}

func canonicalCopilotDocument(raw json.RawMessage) ([]byte, string, error) {
	if len(raw) == 0 || len(raw) > flowCopilotMaxDraftBytes {
		return nil, "", &FlowCopilotInputError{Problem: "el documento debe ser un objeto JSON de hasta 1 MiB"}
	}
	canonical, checksum, err := engine.CanonicalChecksum(raw)
	if err != nil {
		return nil, "", &FlowCopilotInputError{Problem: "el documento no es JSON válido"}
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &object); err != nil || object == nil {
		return nil, "", &FlowCopilotInputError{Problem: "el documento debe ser un objeto JSON"}
	}
	return canonical, checksum, nil
}

func normalizeCopilotJSON(
	raw json.RawMessage,
	fallback []byte,
	want byte,
	max int,
	name string,
) (json.RawMessage, error) {
	if len(raw) == 0 {
		raw = fallback
	}
	if len(raw) > max || !json.Valid(raw) {
		return nil, &FlowCopilotInputError{Problem: name + " debe ser JSON válido dentro del límite permitido"}
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed[0] != want {
		return nil, &FlowCopilotInputError{Problem: name + " tiene una raíz JSON incorrecta"}
	}
	var compact json.RawMessage
	if err := json.Unmarshal(raw, &compact); err == nil {
		// Esta rama no compacta RawMessage (Unmarshal copia los bytes); Marshal sí.
		if normalized, marshalErr := json.Marshal(compact); marshalErr == nil {
			return normalized, nil
		}
	}
	return append(json.RawMessage(nil), raw...), nil
}

// normalizeCopilotToolTrace impide que un adapter persista por accidente los
// argumentos/resultados de function calls. La auditoría conserva solo actividad
// saneada: paso, nombre, estado e id opaco de llamada.
func normalizeCopilotToolTrace(raw json.RawMessage) (json.RawMessage, error) {
	normalized, err := normalizeCopilotJSON(raw, []byte(`[]`), '[', flowCopilotMaxStructuredBytes, "toolTrace")
	if err != nil {
		return nil, err
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(normalized, &items); err != nil {
		return nil, &FlowCopilotInputError{Problem: "toolTrace debe ser un array de actividades"}
	}
	allowed := map[string]bool{"step": true, "name": true, "status": true, "callId": true}
	for _, item := range items {
		for key := range item {
			if !allowed[key] {
				return nil, &FlowCopilotInputError{Problem: "toolTrace contiene campos no permitidos"}
			}
		}
		var step int
		var name, status string
		if json.Unmarshal(item["step"], &step) != nil || step < 1 ||
			json.Unmarshal(item["name"], &name) != nil || strings.TrimSpace(name) == "" ||
			json.Unmarshal(item["status"], &status) != nil || strings.TrimSpace(status) == "" {
			return nil, &FlowCopilotInputError{Problem: "toolTrace contiene una actividad inválida"}
		}
	}
	return normalized, nil
}

func copilotConflict(code, problem string) *FlowCopilotConflictError {
	return &FlowCopilotConflictError{Code: code, Problem: problem}
}

func translateFlowCopilotConstraint(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return err
	}
	switch pgErr.ConstraintName {
	case "uq_flow_copilot_turns_running_user":
		return copilotConflict("turn_already_running", "ya hay un turno del Copilot en ejecución para este usuario")
	case "uq_flow_copilot_proposals_pending_session":
		return copilotConflict("proposal_already_pending", "la sesión ya tiene una propuesta pendiente")
	default:
		return err
	}
}
