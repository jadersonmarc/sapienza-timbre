package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-timbre/db/migrations"
	"github.com/jadersonmarc/sapienza-timbre/internal/api"
	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/chain"
	"github.com/jadersonmarc/sapienza-timbre/internal/notify"
	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
	"github.com/jadersonmarc/sapienza-timbre/internal/producer"
	"github.com/jadersonmarc/sapienza-timbre/internal/testutil"
	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
	"github.com/jadersonmarc/sapienza-timbre/internal/wallet"
)

const adminToken = "test-admin"

func setup(t *testing.T) (*httptest.Server, *pgxpool.Pool) {
	ts, pool, _ := setupSigned(t)
	return ts, pool
}

func setupSigned(t *testing.T) (*httptest.Server, *pgxpool.Pool, *ticketing.Signer) {
	return setupCore(t, chain.NoopChainDriver{})
}

func setupCore(t *testing.T, chainDriver chain.ChainDriver) (*httptest.Server, *pgxpool.Pool, *ticketing.Signer) {
	t.Helper()
	pool := testutil.Pool(t)
	runner := tenancy.NewMigrationRunner(pool, migrations.Tenant)
	signer := ticketing.GenerateSigner()
	srv := api.NewServer(pool, auth.New("test-secret"), producer.New(pool, runner), signer, adminToken, "", api.Seams{
		Chain:   chainDriver,
		Payment: payment.NewFakeGateway(),
		Wallet:  wallet.NoopWalletProvider{},
		Notify:  notify.NewLogNotifier(),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, pool, signer
}

// do faz uma requisição JSON e devolve status + corpo decodificado.
func do(t *testing.T, ts *httptest.Server, method, path string, headers map[string]string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ts.URL+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// seedAdmin cria um operador da plataforma (papel role) e devolve o header Bearer com o
// JWT de admin (escopo "admin"). Substituiu o X-Admin-Token nos testes de /admin.
func seedAdmin(t *testing.T, ts *httptest.Server, pool *pgxpool.Pool, email, role string) map[string]string {
	t.Helper()
	hash, err := auth.HashPassword("senha1234")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO admins (email, password_hash, role) VALUES ($1,$2,$3)
		ON CONFLICT (email) DO UPDATE SET role=EXCLUDED.role`, email, hash, role); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	code, body := do(t, ts, "POST", "/api/v1/admin/login", nil,
		map[string]any{"email": email, "password": "senha1234"})
	if code != http.StatusOK {
		t.Fatalf("admin login: %d %v", code, body)
	}
	return bearer(body["token"].(string))
}

// createProducer cria um produtor e devolve (producerID, ownerToken).
func createProducer(t *testing.T, ts *httptest.Server, name, email, pass string) (string, string) {
	t.Helper()
	code, body := do(t, ts, "POST", "/api/v1/producers",
		map[string]string{"X-Admin-Token": adminToken},
		map[string]any{"name": name, "owner_email": email, "owner_password": pass})
	if code != http.StatusCreated {
		t.Fatalf("create producer: status %d, body %v", code, body)
	}
	prod, _ := body["producer"].(map[string]any)
	pid, _ := prod["id"].(string)
	if pid == "" {
		t.Fatalf("producer id vazio: %v", body)
	}
	return pid, login(t, ts, email, pass)
}

func login(t *testing.T, ts *httptest.Server, email, pass string) string {
	t.Helper()
	code, body := do(t, ts, "POST", "/api/v1/auth/login", nil,
		map[string]any{"email": email, "password": pass})
	if code != http.StatusOK {
		t.Fatalf("login %s: status %d, body %v", email, code, body)
	}
	tok, _ := body["token"].(string)
	if tok == "" {
		t.Fatalf("token vazio no login de %s", email)
	}
	return tok
}

func TestProducerAuthFlow(t *testing.T) {
	ts, _ := setup(t)

	// Gate de admin: sem token e com token errado → recusado.
	if code, _ := do(t, ts, "POST", "/api/v1/producers", nil,
		map[string]any{"name": "X", "owner_email": "a@x.com", "owner_password": "senha1234"}); code != http.StatusUnauthorized {
		t.Fatalf("sem admin token: esperava 401, veio %d", code)
	}
	if code, _ := do(t, ts, "POST", "/api/v1/producers",
		map[string]string{"X-Admin-Token": "errado"}, map[string]any{"name": "X", "owner_email": "a@x.com", "owner_password": "senha1234"}); code != http.StatusUnauthorized {
		t.Fatalf("admin token errado: esperava 401, veio %d", code)
	}

	_, token := createProducer(t, ts, "Casa X", "owner@x.com", "senha1234")

	// /me prova a autenticação: owner com o produtor certo.
	code, body := do(t, ts, "GET", "/api/v1/me", bearer(token), nil)
	if code != http.StatusOK {
		t.Fatalf("/me: status %d, body %v", code, body)
	}
	collab, _ := body["collaborator"].(map[string]any)
	if collab["is_owner"] != true {
		t.Fatalf("/me: esperava is_owner=true, veio %v", collab["is_owner"])
	}
	prod, _ := body["producer"].(map[string]any)
	if prod["name"] != "Casa X" {
		t.Fatalf("/me: esperava producer.name=Casa X, veio %v", prod["name"])
	}

	// Senha errada não autentica.
	if code, _ := do(t, ts, "POST", "/api/v1/auth/login", nil,
		map[string]any{"email": "owner@x.com", "password": "errada__"}); code != http.StatusUnauthorized {
		t.Fatalf("senha errada: esperava 401, veio %d", code)
	}
}

func TestPermissionGate(t *testing.T) {
	ts, _ := setup(t)
	_, ownerToken := createProducer(t, ts, "Casa Y", "owner@y.com", "senha1234")

	// Colaborador COM relatorios e colaborador SEM permissão.
	if code, body := do(t, ts, "POST", "/api/v1/collaborators", bearer(ownerToken),
		map[string]any{"email": "rel@y.com", "password": "senha1234", "permissions": []string{"relatorios"}}); code != http.StatusCreated {
		t.Fatalf("criar colaborador rel: status %d, body %v", code, body)
	}
	if code, body := do(t, ts, "POST", "/api/v1/collaborators", bearer(ownerToken),
		map[string]any{"email": "sem@y.com", "password": "senha1234", "permissions": []string{}}); code != http.StatusCreated {
		t.Fatalf("criar colaborador sem: status %d, body %v", code, body)
	}

	relToken := login(t, ts, "rel@y.com", "senha1234")
	semToken := login(t, ts, "sem@y.com", "senha1234")

	// GET /collaborators exige permissão 'relatorios' (owner passa sempre).
	if code, _ := do(t, ts, "GET", "/api/v1/collaborators", bearer(ownerToken), nil); code != http.StatusOK {
		t.Fatalf("owner listar: esperava 200, veio %d", code)
	}
	if code, _ := do(t, ts, "GET", "/api/v1/collaborators", bearer(relToken), nil); code != http.StatusOK {
		t.Fatalf("rel listar: esperava 200, veio %d", code)
	}
	if code, _ := do(t, ts, "GET", "/api/v1/collaborators", bearer(semToken), nil); code != http.StatusForbidden {
		t.Fatalf("sem permissão listar: esperava 403, veio %d", code)
	}

	// Administração de colaboradores é só do owner.
	if code, _ := do(t, ts, "POST", "/api/v1/collaborators", bearer(relToken),
		map[string]any{"email": "x@y.com", "password": "senha1234"}); code != http.StatusForbidden {
		t.Fatalf("não-owner criar colaborador: esperava 403, veio %d", code)
	}

	// Permissão inválida é rejeitada.
	if code, _ := do(t, ts, "POST", "/api/v1/collaborators", bearer(ownerToken),
		map[string]any{"email": "z@y.com", "password": "senha1234", "permissions": []string{"deus"}}); code != http.StatusBadRequest {
		t.Fatalf("permissão inválida: esperava 400, veio %d", code)
	}
}

// TestTicketSeatInvariant prova que o índice único parcial (não a aplicação) impede
// dois ingressos ativos no mesmo assento.
func TestTicketSeatInvariant(t *testing.T) {
	ts, pool := setup(t)
	pidStr, _ := createProducer(t, ts, "Casa Z", "owner@z.com", "senha1234")
	pid, err := uuid.Parse(pidStr)
	if err != nil {
		t.Fatalf("parse producer id: %v", err)
	}
	ctx := context.Background()

	var eventID, sectorID, seatID, lotID string
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		must(t, tx.QueryRow(ctx, `INSERT INTO events (title, category, category_id)
			VALUES ('E','shows',(SELECT id FROM event_categories WHERE slug='shows')) RETURNING id`).Scan(&eventID))
		must(t, tx.QueryRow(ctx, `INSERT INTO sectors (event_id, name, kind) VALUES ($1,'Plateia','seated') RETURNING id`, eventID).Scan(&sectorID))
		must(t, tx.QueryRow(ctx, `INSERT INTO seats (sector_id, row_label, number) VALUES ($1,'A','1') RETURNING id`, sectorID).Scan(&seatID))
		must(t, tx.QueryRow(ctx, `INSERT INTO lots (event_id, name, price_cents, quantity) VALUES ($1,'Lote 1',1000,100) RETURNING id`, eventID).Scan(&lotID))
		must(t, tx.QueryRow(ctx, `INSERT INTO tickets (event_id, lot_id, seat_id, transferable_after) VALUES ($1,$2,$3, now()) RETURNING id`, eventID, lotID, seatID).Scan(new(string)))
	})

	// Segundo ingresso ATIVO no mesmo assento deve violar o índice único parcial.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, pid); err != nil {
		t.Fatalf("with tenant: %v", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO tickets (event_id, lot_id, seat_id, transferable_after) VALUES ($1,$2,$3, now())`, eventID, lotID, seatID)
	if err == nil {
		t.Fatal("esperava violação de índice único no segundo ingresso ativo, mas passou")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("esperava unique_violation (23505), veio: %v", err)
	}
}

func inTenant(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid uuid.UUID, fn func(tx pgx.Tx)) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, pid); err != nil {
		t.Fatalf("with tenant: %v", err)
	}
	fn(tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("query: %v", err)
	}
}
