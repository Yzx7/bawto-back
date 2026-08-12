package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		fmt.Println("DATABASE_URL missing")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		fmt.Printf("db error: %v\n", err)
		return
	}
	defer pool.Close()

	flowID := "9a490b4f-231e-4611-a2fa-e5faf20009b5"

	var sessions []struct {
		ID        string    `json:"id"`
		Title     string    `json:"title"`
		Status    string    `json:"status"`
		CreatedAt time.Time `json:"createdAt"`
	}
	rows, err := pool.Query(ctx, `SELECT id, title, status, created_at FROM flow_copilot_sessions WHERE flow_id = $1 ORDER BY created_at DESC`, flowID)
	if err != nil {
		fmt.Printf("query sessions: %v\n", err)
		return
	}
	for rows.Next() {
		var s struct {
			ID        string    `json:"id"`
			Title     string    `json:"title"`
			Status    string    `json:"status"`
			CreatedAt time.Time `json:"createdAt"`
		}
		_ = rows.Scan(&s.ID, &s.Title, &s.Status, &s.CreatedAt)
		sessions = append(sessions, s)
	}
	rows.Close()

	fmt.Printf("=== SESSIONS (%d) ===\n", len(sessions))
	for _, s := range sessions {
		fmt.Printf("Session: %s, Title: %s, Status: %s, Created: %s\n", s.ID, s.Title, s.Status, s.CreatedAt.Format(time.RFC3339))

		// Turns
		tRows, _ := pool.Query(ctx, `SELECT id, sequence, user_message, assistant_message, status, mode, editor_revision, persisted_draft_checksum, working_draft_checksum, error_code, created_at, completed_at FROM flow_copilot_turns WHERE session_id = $1 ORDER BY sequence ASC`, s.ID)
		for tRows.Next() {
			var tid, userMsg, status, edRev, pSum, wSum string
			var seq int64
			var asstMsg, mode, errCode *string
			var cAt time.Time
			var compAt *time.Time
			_ = tRows.Scan(&tid, &seq, &userMsg, &asstMsg, &status, &mode, &edRev, &pSum, &wSum, &errCode, &cAt, &compAt)
			fmt.Printf("  Turn #%d (%s): status=%s mode=%v user=%q\n", seq, tid, status, mode, userMsg)
			if asstMsg != nil {
				fmt.Printf("    Assistant (%d chars): %s\n", len(*asstMsg), *asstMsg)
			}
			if errCode != nil {
				fmt.Printf("    ErrorCode: %s\n", *errCode)
			}

			// Proposals for this turn
			pRows, _ := pool.Query(ctx, `SELECT id, status, editor_revision, persisted_base_checksum, working_base_checksum, candidate_checksum, applied_by, applied_at, dismissed_by, candidate, operations, diff, diagnostics FROM flow_copilot_proposals WHERE turn_id = $1`, tid)
			for pRows.Next() {
				var pid, pStatus, pEdRev, pBaseSum, wBaseSum, candSum string
				var appliedBy, dismissedBy *string
				var appliedAt *time.Time
				var cand, ops, diff, diag []byte
				_ = pRows.Scan(&pid, &pStatus, &pEdRev, &pBaseSum, &wBaseSum, &candSum, &appliedBy, &appliedAt, &dismissedBy, &cand, &ops, &diff, &diag)
				fmt.Printf("    Proposal %s: status=%s, edRev=%s, appliedBy=%v, appliedAt=%v\n", pid, pStatus, pEdRev, appliedBy, appliedAt)
				fmt.Printf("      Candidate bytes: %d, ops bytes: %d, diff bytes: %d, diagnostics: %s\n", len(cand), len(ops), len(diff), string(diag))
				if len(cand) > 0 {
					var raw json.RawMessage = cand
					fmt.Printf("      Candidate JSON: %s\n", string(raw))
				}
				if len(ops) > 0 {
					fmt.Printf("      Operations JSON: %s\n", string(ops))
				}
			}
			pRows.Close()
		}
		tRows.Close()
	}
}
