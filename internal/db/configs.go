package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"sesame-bot/internal/models"
)

func GetUserConfig(ctx context.Context, pool *pgxpool.Pool, userID string) (*models.UserConfig, error) {
	c := &models.UserConfig{}
	err := pool.QueryRow(ctx,
		`SELECT id, user_id, sesame_email, sesame_password_enc, headless, weekend,
		        hours_in, hours_out,
		        location_office_lat, location_office_lon,
		        location_home_lat, location_home_lon,
		        office_days, created_at, updated_at
		 FROM user_configs WHERE user_id=$1`,
		userID,
	).Scan(
		&c.ID, &c.UserID, &c.SesameEmail, &c.SesamePasswordEnc,
		&c.Headless, &c.Weekend,
		&c.HoursIn, &c.HoursOut,
		&c.LocationOfficeLat, &c.LocationOfficeLon,
		&c.LocationHomeLat, &c.LocationHomeLon,
		&c.OfficeDays, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("obtener config de usuario: %w", err)
	}
	return c, nil
}

func UpsertUserConfig(ctx context.Context, pool *pgxpool.Pool, c *models.UserConfig) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO user_configs(user_id, sesame_email, sesame_password_enc, headless, weekend,
		    hours_in, hours_out,
		    location_office_lat, location_office_lon,
		    location_home_lat, location_home_lon,
		    office_days, updated_at)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NOW())
		 ON CONFLICT(user_id) DO UPDATE SET
		    sesame_email         = EXCLUDED.sesame_email,
		    sesame_password_enc  = EXCLUDED.sesame_password_enc,
		    headless             = EXCLUDED.headless,
		    weekend              = EXCLUDED.weekend,
		    hours_in             = EXCLUDED.hours_in,
		    hours_out            = EXCLUDED.hours_out,
		    location_office_lat  = EXCLUDED.location_office_lat,
		    location_office_lon  = EXCLUDED.location_office_lon,
		    location_home_lat    = EXCLUDED.location_home_lat,
		    location_home_lon    = EXCLUDED.location_home_lon,
		    office_days          = EXCLUDED.office_days,
		    updated_at           = NOW()`,
		c.UserID, c.SesameEmail, c.SesamePasswordEnc,
		c.Headless, c.Weekend,
		c.HoursIn, c.HoursOut,
		c.LocationOfficeLat, c.LocationOfficeLon,
		c.LocationHomeLat, c.LocationHomeLon,
		c.OfficeDays,
	)
	return err
}

func UpdateSesamePassword(ctx context.Context, pool *pgxpool.Pool, userID, encryptedPw string) error {
	_, err := pool.Exec(ctx,
		`UPDATE user_configs SET sesame_password_enc=$1, updated_at=NOW() WHERE user_id=$2`,
		encryptedPw, userID,
	)
	return err
}

func ClearSesamePassword(ctx context.Context, pool *pgxpool.Pool, userID string) error {
	_, err := pool.Exec(ctx,
		`UPDATE user_configs SET sesame_password_enc='', updated_at=NOW() WHERE user_id=$1`,
		userID,
	)
	return err
}

func GetDayOverrides(ctx context.Context, pool *pgxpool.Pool, userID string) ([]models.DayOverride, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, user_id, weekday, hours_in, hours_out FROM day_overrides WHERE user_id=$1`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var overrides []models.DayOverride
	for rows.Next() {
		var o models.DayOverride
		if err := rows.Scan(&o.ID, &o.UserID, &o.Weekday, &o.HoursIn, &o.HoursOut); err != nil {
			return nil, err
		}
		overrides = append(overrides, o)
	}
	return overrides, rows.Err()
}

func UpsertDayOverride(ctx context.Context, pool *pgxpool.Pool, o *models.DayOverride) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO day_overrides(user_id, weekday, hours_in, hours_out)
		 VALUES($1,$2,$3,$4)
		 ON CONFLICT(user_id, weekday) DO UPDATE SET
		    hours_in  = EXCLUDED.hours_in,
		    hours_out = EXCLUDED.hours_out`,
		o.UserID, o.Weekday, o.HoursIn, o.HoursOut,
	)
	return err
}

func DeleteDayOverride(ctx context.Context, pool *pgxpool.Pool, userID string, weekday int) error {
	_, err := pool.Exec(ctx,
		`DELETE FROM day_overrides WHERE user_id=$1 AND weekday=$2`,
		userID, weekday,
	)
	return err
}
