package models

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ListScheduledBots obtiene solamente bots cuyo flow actual declara un trigger
// schedule. No carga secretos del canal.
func ListScheduledBots(ctx context.Context, pool *pgxpool.Pool) ([]Bot, error) {
	rows, err := pool.Query(ctx, `SELECT `+botCols+` FROM bots
        WHERE flow -> 'trigger' ->> 'type' = 'schedule' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Bot])
}
