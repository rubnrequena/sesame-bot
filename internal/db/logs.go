package db

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"sesame-bot/internal/models"
)

func InsertCheckinLog(ctx context.Context, pool *pgxpool.Pool, userID, action, status, message string, scheduledAt time.Time, source string) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO checkin_logs(user_id, action, status, message, scheduled_at, source)
		 VALUES($1,$2,$3,$4,$5,$6)`,
		userID, action, status, message, scheduledAt, source,
	)
	return err
}

// HasCheckinToday informa si el usuario ya tiene un fichaje con la acción dada y
// estado 'ok' registrado hoy. El inicio del día se calcula con la fecha local del
// proceso para que coincida con el reloj que usa el scheduler.
func HasCheckinToday(ctx context.Context, pool *pgxpool.Pool, userID, action string) (bool, error) {
	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var exists bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS(
		   SELECT 1 FROM checkin_logs
		   WHERE user_id=$1 AND action=$2 AND status='ok' AND executed_at >= $3
		 )`,
		userID, action, startOfDay,
	).Scan(&exists)
	return exists, err
}

func GetUserLogs(ctx context.Context, pool *pgxpool.Pool, userID string, limit, offset int) ([]models.CheckinLog, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, user_id, action, status, message, source, scheduled_at, executed_at
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
		if err := rows.Scan(&l.ID, &l.UserID, &l.Action, &l.Status, &l.Message, &l.Source, &l.ScheduledAt, &l.ExecutedAt); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

func GetAllUsersRecentLogs(ctx context.Context, pool *pgxpool.Pool, limit int) ([]models.CheckinLog, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, user_id, action, status, message, source, scheduled_at, executed_at
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
		if err := rows.Scan(&l.ID, &l.UserID, &l.Action, &l.Status, &l.Message, &l.Source, &l.ScheduledAt, &l.ExecutedAt); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}
