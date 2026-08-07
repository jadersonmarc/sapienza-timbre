// Package db conecta ao Postgres e aplica a camada compartilhada do Timbre (schema
// `public`) no boot. As migrations por-tenant são aplicadas pelo kit/tenancy, não
// aqui. Runner deliberadamente pequeno (sem golang-migrate), espelhando a Margot.
package db

import (
	"context"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect abre um pool pgx.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	return pool, nil
}

// MigratePublic aplica as migrations da camada compartilhada (`public`) uma vez
// cada, rastreadas em public._migrations. Idempotente; seguro a cada boot.
func MigratePublic(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) error {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return fmt.Errorf("glob public migrations: %w", err)
	}
	sort.Strings(names)

	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS public._migrations (
		name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("ensure public._migrations: %w", err)
	}

	for _, name := range names {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM public._migrations WHERE name = $1)`, name,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check public migration %s: %w", name, err)
		}
		if exists {
			continue
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read public migration %s: %w", name, err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply public migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public._migrations (name) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}
