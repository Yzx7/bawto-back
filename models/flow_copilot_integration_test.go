package models

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

func allowCopilotCandidate(context.Context, pgx.Tx, string, string, json.RawMessage) error {
	return nil
}

func TestFlowCopilotApplyUndoAislamientoYReconciliacion(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "flcop_")
	base := grafoValido("f_copilot", "Copilot", "base")
	flow, err := CreateFlow(ctx, pool, bot.ID, NewFlow{
		Key: "copilot", Name: "Copilot", TriggerType: "message", Draft: base, UserID: "author",
	})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	baseSnapshot, err := DraftSnapshotFromFlow(flow)
	if err != nil {
		t.Fatalf("DraftSnapshotFromFlow: %v", err)
	}
	session, err := CreateFlowCopilotSession(ctx, pool, CreateFlowCopilotSessionParams{
		OrganizationID: bot.OrgID, BotID: bot.ID, FlowID: flow.ID, CreatedBy: "author",
		PersistedDraftChecksum: baseSnapshot.Checksum, WorkingDraft: base, EditorRevision: "rev-1",
	})
	if err != nil || session == nil {
		t.Fatalf("CreateFlowCopilotSession: session=%+v err=%v", session, err)
	}
	if foreign, err := GetFlowCopilotSession(ctx, pool, bot.OrgID, bot.ID, flow.ID, session.ID, "other"); err != nil || foreign != nil {
		t.Fatalf("otra identidad vio la sesión privada: detail=%+v err=%v", foreign, err)
	}

	turn, err := CreateFlowCopilotTurn(ctx, pool, CreateFlowCopilotTurnParams{
		OrganizationID: bot.OrgID, BotID: bot.ID, FlowID: flow.ID, SessionID: session.ID,
		CreatedBy: "author", Message: "cambia el saludo", PersistedDraftChecksum: baseSnapshot.Checksum,
		WorkingDraft: base, EditorRevision: "rev-1",
	})
	if err != nil {
		t.Fatalf("CreateFlowCopilotTurn: %v", err)
	}
	candidate := grafoValido("f_copilot", "Copilot", "candidato")
	completed, err := CompleteFlowCopilotTurn(ctx, pool, CompleteFlowCopilotTurnParams{
		OrganizationID: bot.OrgID, BotID: bot.ID, FlowID: flow.ID, SessionID: session.ID,
		TurnID: turn.ID, CreatedBy: "author", AssistantMessage: "Preparé el cambio.", Mode: "proposal",
		Proposal: &NewFlowCopilotProposal{
			PersistedBase: base, WorkingBase: base, EditorRevision: "rev-1", Candidate: candidate,
			Diff: json.RawMessage(`{"nodesModified":1}`),
		},
		ValidateCandidate: allowCopilotCandidate,
	})
	if err != nil || completed == nil || completed.Proposal == nil || completed.Proposal.Status != "pending" {
		t.Fatalf("CompleteFlowCopilotTurn: result=%+v err=%v", completed, err)
	}
	proposal := completed.Proposal

	wrongWorking := grafoValido("f_copilot", "Copilot", "edición local distinta")
	if _, err := ApplyFlowCopilotProposal(ctx, pool, ApplyFlowCopilotProposalParams{
		OrganizationID: bot.OrgID, BotID: bot.ID, FlowID: flow.ID, ProposalID: proposal.ID,
		CreatedBy: "author", ExpectedDraftChecksum: baseSnapshot.Checksum,
		ExpectedEditorRevision: "rev-1", CurrentWorkingDraft: wrongWorking,
		ValidateCandidate: allowCopilotCandidate,
	}); err == nil {
		t.Fatal("apply aceptó una copia de trabajo distinta")
	} else {
		var conflict *FlowCopilotConflictError
		if !errors.As(err, &conflict) || conflict.Code != "working_draft_mismatch" {
			t.Fatalf("conflicto de working draft inesperado: %T %v", err, err)
		}
	}

	rejected := errors.New("binding actual inválido")
	if _, err := ApplyFlowCopilotProposal(ctx, pool, ApplyFlowCopilotProposalParams{
		OrganizationID: bot.OrgID, BotID: bot.ID, FlowID: flow.ID, ProposalID: proposal.ID,
		CreatedBy: "author", ExpectedDraftChecksum: baseSnapshot.Checksum,
		ExpectedEditorRevision: "rev-1", CurrentWorkingDraft: base,
		ValidateCandidate: func(context.Context, pgx.Tx, string, string, json.RawMessage) error { return rejected },
	}); !errors.Is(err, rejected) {
		t.Fatalf("apply ignoró el binding validator: %v", err)
	}
	unchanged, _ := GetFlow(ctx, pool, bot.ID, flow.ID)
	unchangedSnapshot, _ := DraftSnapshotFromFlow(unchanged)
	if unchangedSnapshot.Checksum != baseSnapshot.Checksum {
		t.Fatal("el apply inválido dejó un cambio parcial")
	}

	applied, err := ApplyFlowCopilotProposal(ctx, pool, ApplyFlowCopilotProposalParams{
		OrganizationID: bot.OrgID, BotID: bot.ID, FlowID: flow.ID, ProposalID: proposal.ID,
		CreatedBy: "author", ExpectedDraftChecksum: baseSnapshot.Checksum,
		ExpectedEditorRevision: "rev-1", CurrentWorkingDraft: base,
		ValidateCandidate: allowCopilotCandidate,
	})
	if err != nil || applied == nil || applied.Proposal.Status != "applied" ||
		applied.DraftSnapshot.Checksum != proposal.CandidateChecksum {
		t.Fatalf("ApplyFlowCopilotProposal: result=%+v err=%v", applied, err)
	}
	// Retry por proposalId: no reatribuye ni vuelve a escribir.
	retry, err := ApplyFlowCopilotProposal(ctx, pool, ApplyFlowCopilotProposalParams{
		OrganizationID: bot.OrgID, BotID: bot.ID, FlowID: flow.ID, ProposalID: proposal.ID,
		CreatedBy: "author", ExpectedDraftChecksum: baseSnapshot.Checksum,
		ExpectedEditorRevision: "rev-1", CurrentWorkingDraft: base,
		ValidateCandidate: allowCopilotCandidate,
	})
	if err != nil || retry.DraftSnapshot.Checksum != applied.DraftSnapshot.Checksum {
		t.Fatalf("retry apply no idempotente: result=%+v err=%v", retry, err)
	}
	undone, err := UndoFlowCopilotProposal(ctx, pool, UndoFlowCopilotProposalParams{
		OrganizationID: bot.OrgID, BotID: bot.ID, FlowID: flow.ID, ProposalID: proposal.ID,
		CreatedBy: "author", ExpectedDraftChecksum: applied.DraftSnapshot.Checksum,
	})
	if err != nil || undone == nil || undone.Proposal.Status != "undone" ||
		undone.DraftSnapshot.Checksum != baseSnapshot.Checksum {
		t.Fatalf("UndoFlowCopilotProposal: result=%+v err=%v", undone, err)
	}

	// Una propuesta sobre cambios locales sigue pending si esos mismos bytes se
	// guardan manualmente, y se vuelve stale ante cualquier otro guardado.
	local := grafoValido("f_copilot", "Copilot", "cambio local")
	turn2, err := CreateFlowCopilotTurn(ctx, pool, CreateFlowCopilotTurnParams{
		OrganizationID: bot.OrgID, BotID: bot.ID, FlowID: flow.ID, SessionID: session.ID,
		CreatedBy: "author", Message: "continúa", PersistedDraftChecksum: baseSnapshot.Checksum,
		WorkingDraft: local, EditorRevision: "rev-2",
	})
	if err != nil {
		t.Fatalf("Create turn 2: %v", err)
	}
	completed2, err := CompleteFlowCopilotTurn(ctx, pool, CompleteFlowCopilotTurnParams{
		OrganizationID: bot.OrgID, BotID: bot.ID, FlowID: flow.ID, SessionID: session.ID,
		TurnID: turn2.ID, CreatedBy: "author", AssistantMessage: "Otra propuesta.", Mode: "proposal",
		Proposal: &NewFlowCopilotProposal{
			PersistedBase: base, WorkingBase: local, EditorRevision: "rev-2",
			Candidate: grafoValido("f_copilot", "Copilot", "sobre local"),
		},
		ValidateCandidate: allowCopilotCandidate,
	})
	if err != nil || completed2.Proposal == nil {
		t.Fatalf("Complete turn 2: result=%+v err=%v", completed2, err)
	}
	localSnapshot, err := UpdateFlowDraft(ctx, pool, bot.ID, flow.ID, local, baseSnapshot.Checksum, "author")
	if err != nil {
		t.Fatalf("guardar working base: %v", err)
	}
	detail, err := GetFlowCopilotSession(ctx, pool, bot.OrgID, bot.ID, flow.ID, session.ID, "author")
	if err != nil {
		t.Fatalf("GetFlowCopilotSession: %v", err)
	}
	var reconciled *FlowCopilotProposal
	for index := range detail.Proposals {
		if detail.Proposals[index].ID == completed2.Proposal.ID {
			reconciled = &detail.Proposals[index]
		}
	}
	if reconciled == nil || reconciled.Status != "pending" || reconciled.PersistedBaseChecksum != localSnapshot.Checksum {
		t.Fatalf("propuesta no rebasada al guardar su working base: %+v", reconciled)
	}
	other := grafoValido("f_copilot", "Copilot", "otro guardado")
	if _, err := UpdateFlowDraft(ctx, pool, bot.ID, flow.ID, other, localSnapshot.Checksum, "author"); err != nil {
		t.Fatalf("guardar cambio divergente: %v", err)
	}
	detail, _ = GetFlowCopilotSession(ctx, pool, bot.OrgID, bot.ID, flow.ID, session.ID, "author")
	for index := range detail.Proposals {
		if detail.Proposals[index].ID == completed2.Proposal.ID && detail.Proposals[index].Status != "stale" {
			t.Fatalf("propuesta divergente siguió aplicable: %+v", detail.Proposals[index])
		}
	}
}
