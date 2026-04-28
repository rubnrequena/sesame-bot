package db

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Connect(ctx context.Context) (*pgxpool.Pool, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return nil, fmt.Errorf("DATABASE_URL es requerido")
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("conectar a postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping a postgres: %w", err)
	}
	return pool, nil
}

func RunMigrations(ctx context.Context, pool *pgxpool.Pool, migrationsDir string) error {
	_, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)
	if err != nil {
		return fmt.Errorf("crear tabla schema_migrations: %w", err)
	}

	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return fmt.Errorf("leer directorio de migraciones: %w", err)
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, name := range files {
		var applied bool
		err := pool.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)", name,
		).Scan(&applied)
		if err != nil {
			return fmt.Errorf("verificar migración %s: %w", name, err)
		}
		if applied {
			continue
		}

		content, err := os.ReadFile(filepath.Join(migrationsDir, name))
		if err != nil {
			return fmt.Errorf("leer migración %s: %w", name, err)
		}

		sql := string(content)
		// Strip simple migration comments
		if idx := strings.Index(sql, "-- +migrate Up"); idx >= 0 {
			sql = sql[idx+len("-- +migrate Up"):]
		}

		if _, err := pool.Exec(ctx, sql); err != nil {
			return fmt.Errorf("ejecutar migración %s: %w", name, err)
		}

		if _, err := pool.Exec(ctx,
			"INSERT INTO schema_migrations(version) VALUES($1)", name,
		); err != nil {
			return fmt.Errorf("registrar migración %s: %w", name, err)
		}

		log.Printf("Migración aplicada: %s", name)
	}

	return nil
}
