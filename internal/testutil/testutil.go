// Package testutil dá suporte aos testes de integração do Timbre: um pool contra o
// Postgres de TEST_DATABASE_URL, com a camada `public` migrada e um estado limpo a
// cada aquisição. Os testes pulam (t.Skip) quando TEST_DATABASE_URL não está setada,
// e compartilham um só Postgres — rode com `go test -p 1 ./...`.
package testutil

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-timbre/db/migrations"
	"github.com/jadersonmarc/sapienza-timbre/internal/db"
)

// Pool conecta, aplica as migrations de `public` e limpa o estado. Registra um
// cleanup que fecha o pool ao fim do teste.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL não setada — pulando teste de integração")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := db.MigratePublic(ctx, pool, migrations.Public); err != nil {
		pool.Close()
		t.Fatalf("migrate public: %v", err)
	}
	clean(ctx, t, pool)
	t.Cleanup(pool.Close)
	return pool
}

// clean derruba todos os schemas tenant_* e zera as tabelas de controle/identidade,
// para cada teste começar do zero (execução serial garante isolamento).
func clean(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT nspname FROM pg_namespace WHERE nspname LIKE 'tenant\_%' ESCAPE '\'`)
	if err != nil {
		t.Fatalf("list tenant schemas: %v", err)
	}
	var schemas []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			t.Fatalf("scan schema: %v", err)
		}
		schemas = append(schemas, n)
	}
	rows.Close()
	for _, s := range schemas {
		if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS "`+s+`" CASCADE`); err != nil {
			t.Fatalf("drop schema %s: %v", s, err)
		}
	}
	// producers CASCADE zera collaborators e collaborator_permissions.
	if _, err := pool.Exec(ctx, `TRUNCATE producers CASCADE`); err != nil {
		t.Fatalf("truncate producers: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE subjects CASCADE`); err != nil {
		t.Fatalf("truncate subjects: %v", err)
	}
	// Camada do painel /admin: operadores, catálogo global, moderação e auditoria.
	if _, err := pool.Exec(ctx, `TRUNCATE admins, artists, moderation_flags, audit_log CASCADE`); err != nil {
		t.Fatalf("truncate admin: %v", err)
	}
	// Notificações (públicas, assíncronas) e índices de sessão de checkout.
	if _, err := pool.Exec(ctx, `TRUNCATE notifications, checkout_session_index CASCADE`); err != nil {
		t.Fatalf("truncate notifications: %v", err)
	}
	// OTP de comprador (sem FK; acumularia cooldown entre execuções).
	if _, err := pool.Exec(ctx, `TRUNCATE buyer_otps`); err != nil {
		t.Fatalf("truncate buyer_otps: %v", err)
	}
}
