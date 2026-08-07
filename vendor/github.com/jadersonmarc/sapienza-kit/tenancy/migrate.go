package tenancy

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MigrationRunner applies a product's tenant migrations to one tenant schema or
// to every provisioned tenant. Migrations are plain .up.sql files named
// "NNNN_name.up.sql" (e.g. 0001_init.up.sql) in an fs.FS the product embeds.
// Applied versions are tracked in a schema_migrations table inside each tenant
// schema, so re-running is idempotent.
//
// A deliberately small runner (no golang-migrate) because per-schema migration
// is simpler to reason about than driver-level search_path juggling, and the
// product owns only forward .up.sql for tenant tables.
type MigrationRunner struct {
	pool *pgxpool.Pool
	fsys fs.FS
}

// NewMigrationRunner builds a runner over the given pool and migrations FS.
func NewMigrationRunner(pool *pgxpool.Pool, fsys fs.FS) *MigrationRunner {
	return &MigrationRunner{pool: pool, fsys: fsys}
}

type migration struct {
	version int64
	name    string
	sql     string
}

// load reads and sorts all *.up.sql migrations from the FS.
func (r *MigrationRunner) load() ([]migration, error) {
	entries, err := fs.Glob(r.fsys, "*.up.sql")
	if err != nil {
		return nil, fmt.Errorf("glob migrations: %w", err)
	}
	migs := make([]migration, 0, len(entries))
	for _, name := range entries {
		base := strings.TrimSuffix(name, ".up.sql")
		idx := strings.IndexByte(base, '_')
		if idx <= 0 {
			return nil, fmt.Errorf("migration %q must be NNNN_name.up.sql", name)
		}
		version, err := strconv.ParseInt(base[:idx], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration %q has non-numeric version: %w", name, err)
		}
		body, err := fs.ReadFile(r.fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", name, err)
		}
		migs = append(migs, migration{version: version, name: base, sql: string(body)})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	return migs, nil
}

// ApplyToTenant applies all pending migrations to the tenant's schema. Each
// migration runs in its own transaction scoped to the tenant via WithTenant.
func (r *MigrationRunner) ApplyToTenant(ctx context.Context, tenantID uuid.UUID) error {
	migs, err := r.load()
	if err != nil {
		return err
	}
	for _, m := range migs {
		if err := r.applyOne(ctx, tenantID, m); err != nil {
			return err
		}
	}
	return nil
}

func (r *MigrationRunner) applyOne(ctx context.Context, tenantID uuid.UUID, m migration) (err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", m.name, err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	if err = WithTenant(ctx, tx, tenantID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version bigint PRIMARY KEY,
		name text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, m.version).Scan(&exists); err != nil {
		return fmt.Errorf("check migration %d: %w", m.version, err)
	}
	if exists {
		return tx.Commit(ctx)
	}

	if _, err = tx.Exec(ctx, m.sql); err != nil {
		return fmt.Errorf("apply migration %s: %w", m.name, err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.version, m.name); err != nil {
		return fmt.Errorf("record migration %s: %w", m.name, err)
	}
	return tx.Commit(ctx)
}

// ApplyToAllTenants applies pending migrations to every provisioned tenant
// schema (every schema named tenant_*). Useful for rolling out a new product
// migration across existing tenants.
func (r *MigrationRunner) ApplyToAllTenants(ctx context.Context) error {
	schemas, err := ListTenantSchemas(ctx, r.pool)
	if err != nil {
		return err
	}
	for _, tid := range schemas {
		if err := r.ApplyToTenant(ctx, tid); err != nil {
			return err
		}
	}
	return nil
}

// ListTenantSchemas returns the tenant UUIDs for every tenant_* schema present.
func ListTenantSchemas(ctx context.Context, pool *pgxpool.Pool) ([]uuid.UUID, error) {
	rows, err := pool.Query(ctx, `SELECT nspname FROM pg_namespace WHERE nspname LIKE 'tenant\_%' ESCAPE '\'`)
	if err != nil {
		return nil, fmt.Errorf("list tenant schemas: %w", err)
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		hex := strings.TrimPrefix(name, "tenant_")
		if len(hex) != 32 {
			continue
		}
		// Re-insert hyphens to parse back into a UUID.
		canonical := hex[0:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:32]
		tid, err := uuid.Parse(canonical)
		if err != nil {
			continue
		}
		out = append(out, tid)
	}
	return out, rows.Err()
}
