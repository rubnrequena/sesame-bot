package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"sesame-bot/internal/models"
)

const sessionTTL = 24 * time.Hour

func CreateSession(ctx context.Context, pool *pgxpool.Pool, userID string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generar token: %w", err)
	}
	token := hex.EncodeToString(b)
	expiresAt := time.Now().Add(sessionTTL)

	_, err := pool.Exec(ctx,
		`INSERT INTO sessions(token, user_id, expires_at) VALUES($1,$2,$3)`,
		token, userID, expiresAt,
	)
	if err != nil {
		return "", fmt.Errorf("crear sesión: %w", err)
	}
	return token, nil
}

func GetSessionUser(ctx context.Context, pool *pgxpool.Pool, token string) (*models.User, error) {
	u := &models.User{}
	err := pool.QueryRow(ctx,
		`SELECT u.id, u.email, u.password_hash, u.is_admin, u.is_active, u.created_at, u.updated_at
		 FROM sessions s
		 JOIN users u ON u.id = s.user_id
		 WHERE s.token=$1 AND s.expires_at > NOW() AND u.is_active = TRUE`,
		token,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.IsAdmin, &u.IsActive, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func DeleteSession(ctx context.Context, pool *pgxpool.Pool, token string) error {
	_, err := pool.Exec(ctx, `DELETE FROM sessions WHERE token=$1`, token)
	return err
}

func CleanExpiredSessions(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= NOW()`)
	return err
}
