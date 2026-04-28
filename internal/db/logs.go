package db

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"sesame-bot/internal/models"
)

func InsertCheckinLog(ctx context.Context, pool *pgxpool.Pool, userID, action, status, message string, scheduledAt time.Time) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO checkin_logs(user_id, action, status, message, scheduled_at)
		 VALUES($1,$2,$3,$4,$5)`,
		userID, action, status, message, scheduledAt,
	)
	return err
}

func GetUserLogs(ctx context.Context, pool *pgxpool.Pool, userID string, limit, offset int) ([]models.CheckinLog, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, user_id, action, status, message, scheduled_at, executed_at
		 FROM checkin_logs
		 WHERE user_id=$1
		 ORDER BY executed_at DESC
		 LIMIT $2 OFFSET $3`,
		userID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []models.CheckinLog
	for rows.Next() {
		var l models.CheckinLog
		if err := rows.Scan(&l.ID, &l.UserID, &l.Action, &l.Status, &l.Message, &l.ScheduledAt, &l.ExecutedAt); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

func GetAllUsersRecentLogs(ctx context.Context, pool *pgxpool.Pool, limit int) ([]models.CheckinLog, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, user_id, action, status, message, scheduled_at, executed_at
		 FROM checkin_logs
		 ORDER BY executed_at DESC
		 LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []models.CheckinLog
	for rows.Next() {
		var l models.CheckinLog
		if err := rows.Scan(&l.ID, &l.UserID, &l.Action, &l.Status, &l.Message, &l.ScheduledAt, &l.ExecutedAt); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}
