package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"sesame-bot/internal/models"
)

func CountUsers(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var count int
	err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM users").Scan(&count)
	return count, err
}

func CreateUser(ctx context.Context, pool *pgxpool.Pool, email, passwordHash string, isAdmin bool) (*models.User, error) {
	u := &models.User{}
	err := pool.QueryRow(ctx,
		`INSERT INTO users(email, password_hash, is_admin)
		 VALUES($1, $2, $3)
		 RETURNING id, email, password_hash, is_admin, is_active, created_at, updated_at`,
		email, passwordHash, isAdmin,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.IsAdmin, &u.IsActive, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("crear usuario: %w", err)
	}
	// Create empty config row
	_, err = pool.Exec(ctx,
		`INSERT INTO user_configs(user_id) VALUES($1) ON CONFLICT(user_id) DO NOTHING`, u.ID)
	if err != nil {
		return nil, fmt.Errorf("crear config de usuario: %w", err)
	}
	return u, nil
}

func GetUserByEmail(ctx context.Context, pool *pgxpool.Pool, email string) (*models.User, error) {
	u := &models.User{}
	err := pool.QueryRow(ctx,
		`SELECT id, email, password_hash, is_admin, is_active, created_at, updated_at
		 FROM users WHERE email=$1`,
		email,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.IsAdmin, &u.IsActive, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func GetUserByID(ctx context.Context, pool *pgxpool.Pool, id string) (*models.User, error) {
	u := &models.User{}
	err := pool.QueryRow(ctx,
		`SELECT id, email, password_hash, is_admin, is_active, created_at, updated_at
		 FROM users WHERE id=$1`,
		id,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.IsAdmin, &u.IsActive, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func ListUsers(ctx context.Context, pool *pgxpool.Pool) ([]models.User, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, email, password_hash, is_admin, is_active, created_at, updated_at
		 FROM users ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []models.User
	for rows.Next() {
		var u models.User
		if err := rows.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.IsAdmin, &u.IsActive, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func ToggleUserActive(ctx context.Context, pool *pgxpool.Pool, id string) error {
	_, err := pool.Exec(ctx,
		`UPDATE users SET is_active = NOT is_active, updated_at = NOW() WHERE id=$1`, id)
	return err
}

func LoadActiveUsersWithConfig(ctx context.Context, pool *pgxpool.Pool) ([]models.UserWithConfig, error) {
	rows, err := pool.Query(ctx,
		`SELECT u.id, u.email, u.is_admin, u.is_active, u.created_at, u.updated_at,
		        c.id, c.sesame_email, c.sesame_password_enc, c.headless, c.weekend,
		        c.hours_in, c.hours_out,
		        c.location_office_lat, c.location_office_lon,
		        c.location_home_lat, c.location_home_lon,
		        c.office_days
		 FROM users u
		 JOIN user_configs c ON c.user_id = u.id
		 WHERE u.is_active = TRUE
		   AND c.sesame_email != ''
		   AND c.hours_in != ''
		   AND c.hours_out != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.UserWithConfig
	for rows.Next() {
		var uw models.UserWithConfig
		u := &uw.User
		c := &uw.Config
		if err := rows.Scan(
			&u.ID, &u.Email, &u.IsAdmin, &u.IsActive, &u.CreatedAt, &u.UpdatedAt,
			&c.ID, &c.SesameEmail, &c.SesamePasswordEnc, &c.Headless, &c.Weekend,
			&c.HoursIn, &c.HoursOut,
			&c.LocationOfficeLat, &c.LocationOfficeLon,
			&c.LocationHomeLat, &c.LocationHomeLon,
			&c.OfficeDays,
		); err != nil {
			return nil, err
		}
		c.UserID = u.ID
		result = append(result, uw)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Load day overrides for each user
	for i, uw := range result {
		overrides, err := GetDayOverrides(ctx, pool, uw.User.ID)
		if err != nil {
			return nil, err
		}
		result[i].DayOverrides = overrides
	}

	return result, nil
}
