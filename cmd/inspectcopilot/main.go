package main

import (
	"context"
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

	flowID := "c2b5a88e-fbfd-4e01-9004-3fa0fe7f8c00"
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "-flow-id" && i+1 < len(os.Args) {
			flowID = os.Args[i+1]
			i++
		}
	}

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

	// Flow draft info
	var fDraft []byte
	err = pool.QueryRow(ctx, `SELECT draft FROM flows WHERE id = $1`, flowID).Scan(&fDraft)
	if err != nil {
		fmt.Printf("flow draft error: %v\n", err)
	} else {
		fmt.Printf("Flow draft bytes: %d\n", len(fDraft))
	}

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
			fmt.Printf("    edRev=%s, pSum=%s, wSum=%s\n", edRev, pSum, wSum)

			// Proposals for this turn
			pRows, _ := pool.Query(ctx, `SELECT id, status, editor_revision, persisted_base_checksum, working_base_checksum, candidate_checksum, requirements, assumptions, diagnostics FROM flow_copilot_proposals WHERE turn_id = $1`, tid)
			for pRows.Next() {
				var pid, pStatus, pEdRev, pBaseSum, wBaseSum, candSum string
				var reqs, assump, diag []byte
				_ = pRows.Scan(&pid, &pStatus, &pEdRev, &pBaseSum, &wBaseSum, &candSum, &reqs, &assump, &diag)
				fmt.Printf("    Proposal %s: status=%s, edRev=%s\n", pid, pStatus, pEdRev)
				fmt.Printf("      pBaseSum: %s\n", pBaseSum)
				fmt.Printf("      wBaseSum: %s\n", wBaseSum)
				fmt.Printf("      candSum:  %s\n", candSum)
				fmt.Printf("      requirements: %s\n", string(reqs))
				fmt.Printf("      assumptions:  %s\n", string(assump))
				fmt.Printf("      diagnostics:  %s\n", string(diag))
			}
			pRows.Close()
		}
		tRows.Close()
	}
}
