package models

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ReminderCorrelation struct {
	Method            string  `json:"method"`
	OutboundMessageID *int64  `json:"outboundMessageId,omitempty"`
	FlowRunID         *string `json:"flowRunId,omitempty"`
	DataRecordID      *string `json:"dataRecordId,omitempty"`
	CandidateCount    int     `json:"candidateCount"`
}

type correlationCandidate struct {
	OutboundMessageID int64
	FlowRunID         string
	DataRecordID      string
}

// CorrelateInboundReminder prefiere el mensaje citado. Sin cita solo infiere
// cuando todos los recordatorios recientes apuntan al mismo registro.
func CorrelateInboundReminder(ctx context.Context, p *pgxpool.Pool, inboundMessageID int64, chatID, quotedProviderID string, window time.Duration) (*ReminderCorrelation, error) {
	if window <= 0 {
		window = 72 * time.Hour
	}
	if quotedProviderID != "" {
		var candidate correlationCandidate
		err := p.QueryRow(ctx, `SELECT m.id,r.id::text,r.data_record_id::text
			FROM messages m JOIN flow_runs r ON r.provider_message_id=m.wa_id
			WHERE m.chat_id=$1::uuid AND m.wa_id=$2 AND r.data_record_id IS NOT NULL`,
			chatID, quotedProviderID).Scan(&candidate.OutboundMessageID, &candidate.FlowRunID, &candidate.DataRecordID)
		if err == nil {
			result := &ReminderCorrelation{
				Method: "exact", OutboundMessageID: &candidate.OutboundMessageID,
				FlowRunID: &candidate.FlowRunID, DataRecordID: &candidate.DataRecordID,
				CandidateCount: 1,
			}
			return result, saveMessageCorrelation(ctx, p, inboundMessageID, quotedProviderID, result)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		result := &ReminderCorrelation{Method: "none"}
		return result, saveMessageCorrelation(ctx, p, inboundMessageID, quotedProviderID, result)
	}

	rows, err := p.Query(ctx, `SELECT m.id,r.id::text,r.data_record_id::text
		FROM messages m JOIN flow_runs r ON r.provider_message_id=m.wa_id
		WHERE m.chat_id=$1::uuid AND m.from_me
		  AND r.status IN ('sent','delivered','read','played')
		  AND r.data_record_id IS NOT NULL
		  AND m.created_at >= NOW()-make_interval(secs => $2)
		ORDER BY m.created_at DESC,m.id DESC`, chatID, int64(window/time.Second))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byRecord := map[string]correlationCandidate{}
	order := make([]string, 0)
	for rows.Next() {
		var candidate correlationCandidate
		if err := rows.Scan(&candidate.OutboundMessageID, &candidate.FlowRunID, &candidate.DataRecordID); err != nil {
			return nil, err
		}
		if _, exists := byRecord[candidate.DataRecordID]; !exists {
			byRecord[candidate.DataRecordID] = candidate
			order = append(order, candidate.DataRecordID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := &ReminderCorrelation{Method: "none", CandidateCount: len(byRecord)}
	if len(byRecord) == 1 {
		candidate := byRecord[order[0]]
		result.Method = "inferred"
		result.OutboundMessageID = &candidate.OutboundMessageID
		result.FlowRunID = &candidate.FlowRunID
		result.DataRecordID = &candidate.DataRecordID
	} else if len(byRecord) > 1 {
		result.Method = "ambiguous"
	}
	return result, saveMessageCorrelation(ctx, p, inboundMessageID, "", result)
}

func saveMessageCorrelation(ctx context.Context, p *pgxpool.Pool, inboundMessageID int64, quotedProviderID string, result *ReminderCorrelation) error {
	_, err := p.Exec(ctx, `INSERT INTO message_correlations
		(inbound_message_id,outbound_message_id,flow_run_id,data_record_id,method,
		 quoted_provider_message_id,candidate_count)
		VALUES($1,$2,$3::uuid,$4::uuid,$5,NULLIF($6,''),$7)
		ON CONFLICT(inbound_message_id) DO NOTHING`,
		inboundMessageID, result.OutboundMessageID, result.FlowRunID, result.DataRecordID,
		result.Method, quotedProviderID, result.CandidateCount)
	return err
}

// CorrelationContext aporta solo variables genéricas; el grafo decide qué
// significado de negocio tiene el registro correlacionado.
func CorrelationContext(ctx context.Context, p *pgxpool.Pool, botID string, correlation *ReminderCorrelation) (map[string]string, error) {
	vars := map[string]string{}
	if correlation == nil {
		return vars, nil
	}
	vars["correlation_method"] = correlation.Method
	if correlation.Method == "ambiguous" {
		vars["correlation_ambiguous"] = "true"
		return vars, nil
	}
	if correlation.DataRecordID == nil {
		return vars, nil
	}
	recordVars, err := DataRecordContext(ctx, p, botID, *correlation.DataRecordID)
	if err != nil {
		return nil, err
	}
	for key, value := range recordVars {
		vars[key] = value
	}
	vars["correlated_record_id"] = *correlation.DataRecordID
	if correlation.FlowRunID != nil {
		vars["correlated_flow_run_id"] = *correlation.FlowRunID
		var replyIntent string
		err := p.QueryRow(ctx, `SELECT COALESCE(v.definition->'trigger'->>'replyIntent','')
			FROM flow_runs r JOIN flow_versions v ON v.id=r.flow_version_id
			WHERE r.id=$1::uuid`, *correlation.FlowRunID).Scan(&replyIntent)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if replyIntent != "" {
			vars["source_intent"] = replyIntent
		}
	}
	return vars, nil
}
