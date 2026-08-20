package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-timbre/internal/chain"
	"github.com/jadersonmarc/sapienza-timbre/internal/testutil"
	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
	"github.com/jadersonmarc/sapienza-timbre/internal/wallet"
)

// setupOnDemand sobe o servidor em CHAIN_MINT_MODE=on_demand com a rede ligada (okChain).
func setupOnDemand(t *testing.T) (*httptest.Server, *pgxpool.Pool) {
	ts, pool, _ := setupCoreMode(t, okChain{}, chain.MintModeOnDemand)
	return ts, pool
}

// buyStanding compra n ingressos de pista (on_demand) e confirma o pagamento.
func buyStanding(t *testing.T, ts *httptest.Server, pool *pgxpool.Pool, owner string, n int) string {
	t.Helper()
	eventID := createEvent(t, ts, owner, "Show OnDemand", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	_, lots := getEventLots(t, ts, owner, eventID)
	_, body := do(t, ts, "POST", "/api/v1/public/checkout", buyer(t, ts, pool, "buy@ondemand.com"), map[string]any{
		"event_id": eventID, "lot_id": lots[0], "quantity": n, "method": "pix",
	})
	confirmWebhook(t, ts, body["asaas_ref"].(string))
	return eventID
}

// ticketToken devolve o QR assinado do primeiro ingresso do tenant.
func ticketToken(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid uuid.UUID) string {
	t.Helper()
	var tok string
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM tickets LIMIT 1`).Scan(&id); err != nil {
			t.Fatalf("ticket: %v", err)
		}
		var err error
		tok, err = ticketing.TicketToken(ctx, tx, id)
		if err != nil {
			t.Fatalf("token: %v", err)
		}
	})
	return tok
}

// TestOnDemandConfirmDoesNotEnqueue: em on_demand, a confirmação do pagamento NÃO enfileira
// mint; o ingresso nasce 'not_materialized' e entra na portaria normalmente.
func TestOnDemandConfirmDoesNotEnqueue(t *testing.T) {
	ts, pool := setupOnDemand(t)
	_, owner := createProducer(t, ts, "Casa OD", "owner@od.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	_ = buyStanding(t, ts, pool, owner, 2)

	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM chain_jobs`); n != 0 {
		t.Fatalf("on_demand não deveria enfileirar mint, veio %d jobs", n)
	}
	if st := scanStr(t, ctx, pool, pid, `SELECT chain_status FROM tickets LIMIT 1`); st != "not_materialized" {
		t.Fatalf("esperava not_materialized, veio %s", st)
	}
	// A portaria segue validando (Ed25519 offline, independente da rede).
	tok := ticketToken(t, ctx, pool, pid)
	if _, vb := do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner), map[string]any{"token": tok, "gate": "G1"}); vb["verdict"] != "admitted" {
		t.Fatalf("portaria: %v", vb)
	}
}

// TestOnDemandResaleListingMaterializes: anunciar materializa (reason=resale_listing) e o
// anúncio fica indisponível enquanto 'pending'; após o mint, a compra anda.
func TestOnDemandResaleListingMaterializes(t *testing.T) {
	ts, pool := setupOnDemand(t)
	_, owner := createProducer(t, ts, "Casa Rev", "owner@rev.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	_ = buyStanding(t, ts, pool, owner, 1)
	tid := firstTicket(t, ctx, pool, pid)

	// Anuncia → materializa (1 job, reason resale_listing).
	code, lbody := do(t, ts, "POST", "/api/v1/tickets/"+tid.String()+"/listings", bearer(owner),
		map[string]any{"price_cents": 4000})
	if code != http.StatusCreated {
		t.Fatalf("anúncio: %d %v", code, lbody)
	}
	listingID, _ := lbody["id"].(string)
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM chain_jobs WHERE reason='resale_listing'`); n != 1 {
		t.Fatalf("esperava 1 job resale_listing, veio %d", n)
	}

	// Antes do mint, a compra é recusada (indisponível até minted).
	if code, _ := do(t, ts, "POST", "/api/v1/public/listings/"+listingID+"/buy", buyer(t, ts, pool, "comprador@rev.com"), map[string]any{}); code != http.StatusConflict {
		t.Fatalf("compra antes do mint: esperava 409, veio %d", code)
	}

	// Mint conclui → compra anda.
	if _, err := chain.ProcessTenant(ctx, pool, okChain{}, pid); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if code, _ := do(t, ts, "POST", "/api/v1/public/listings/"+listingID+"/buy", buyer(t, ts, pool, "comprador@rev.com"), map[string]any{}); code != http.StatusCreated {
		t.Fatalf("compra após mint: esperava 201, veio %d", code)
	}
}

// TestOnDemandExportRequiresImportedAddress: exportar sem endereço importado falha claro.
func TestOnDemandExportRequiresImportedAddress(t *testing.T) {
	ts, pool := setupOnDemand(t)
	_, owner := createProducer(t, ts, "Casa Exp", "owner@exp.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	_ = buyStanding(t, ts, pool, owner, 1)
	tid := firstTicket(t, ctx, pool, pid)
	token := verifyOTP(t, ts, pool, "buy@ondemand.com", "123456")

	code, body := do(t, ts, "POST", "/api/v1/public/me/tickets/"+tid.String()+"/export", bearer(token), nil)
	if code != http.StatusConflict {
		t.Fatalf("export sem endereço importado: esperava 409, veio %d %v", code, body)
	}
}

// TestDeriveAddressDeterministic: determinístico, idempotente por subject e único por índice.
func TestDeriveAddressDeterministic(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	d := wallet.NewDeriver(wallet.StaticSeedProvider{SeedBytes: []byte("semente-deterministica-teste")}, nil)

	// dois subjects
	var s1, s2 uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO subjects DEFAULT VALUES RETURNING id`).Scan(&s1); err != nil {
		t.Fatalf("subject: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO subjects DEFAULT VALUES RETURNING id`).Scan(&s2); err != nil {
		t.Fatalf("subject: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	w1, err := d.DeriveAddress(ctx, tx, s1)
	if err != nil {
		t.Fatalf("derive s1: %v", err)
	}
	w1again, err := d.DeriveAddress(ctx, tx, s1)
	if err != nil {
		t.Fatalf("derive s1 (idempotente): %v", err)
	}
	w2, err := d.DeriveAddress(ctx, tx, s2)
	if err != nil {
		t.Fatalf("derive s2: %v", err)
	}
	if w1.Address != w1again.Address {
		t.Fatalf("derivação não é idempotente: %s vs %s", w1.Address, w1again.Address)
	}
	if w1.Address == w2.Address {
		t.Fatalf("endereços de subjects distintos colidiram: %s", w1.Address)
	}
	// índice único por carteira derivada.
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(DISTINCT derivation_index) FROM wallets WHERE origin='derived'`).Scan(&count); err != nil {
		t.Fatalf("índices: %v", err)
	}
	if count != 2 {
		t.Fatalf("esperava 2 índices distintos, veio %d", count)
	}
}

// TestOnDemandMaterializeGroupsStanding: N ingressos de pista do mesmo lote → 1 job;
// com seat_id, N jobs.
func TestOnDemandMaterializeGroupsStanding(t *testing.T) {
	ts, pool := setupOnDemand(t)
	_, owner := createProducer(t, ts, "Casa Group", "owner@group.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()

	// Pista: 3 ingressos do mesmo lote.
	_ = buyStanding(t, ts, pool, owner, 3)
	var standingIDs []uuid.UUID
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		rows, err := tx.Query(ctx, `SELECT id FROM tickets WHERE seat_id IS NULL`)
		if err != nil {
			t.Fatalf("tickets: %v", err)
		}
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			standingIDs = append(standingIDs, id)
		}
		rows.Close()
	})
	if len(standingIDs) != 3 {
		t.Fatalf("esperava 3 ingressos de pista, veio %d", len(standingIDs))
	}
	materialize(t, ctx, pool, pid, standingIDs, chain.ReasonCollectible)
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM chain_jobs WHERE kind='mint'`); n != 1 {
		t.Fatalf("pista agrupada: esperava 1 job, veio %d", n)
	}
	if amt := scanInt(t, ctx, pool, pid, `SELECT amount FROM chain_jobs WHERE kind='mint' LIMIT 1`); amt != 3 {
		t.Fatalf("pista agrupada: esperava amount 3, veio %d", amt)
	}

	// Assento marcado: 2 ingressos → 2 jobs.
	eventID, seats, lotID := seatedEvent(t, ts, pool, owner, pid, 2)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	_, body := do(t, ts, "POST", "/api/v1/public/checkout", buyer(t, ts, pool, "buy@group.com"), map[string]any{
		"event_id": eventID.String(), "lot_id": lotID.String(), "quantity": 2,
		"seat_ids": []string{seats[0].String(), seats[1].String()}, "method": "pix",
	})
	confirmWebhook(t, ts, body["asaas_ref"].(string))
	var seatedIDs []uuid.UUID
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		rows, err := tx.Query(ctx, `SELECT id FROM tickets WHERE seat_id IS NOT NULL`)
		if err != nil {
			t.Fatalf("tickets: %v", err)
		}
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			seatedIDs = append(seatedIDs, id)
		}
		rows.Close()
	})
	materialize(t, ctx, pool, pid, seatedIDs, chain.ReasonCollectible)
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM chain_jobs WHERE kind='mint' AND ticket_id = ANY($1)`, seatedIDs); n != 2 {
		t.Fatalf("assento marcado: esperava 2 jobs, veio %d", n)
	}
}

// TestBackfillMaterializeRespectsLimit: backfill emite o histórico respeitando o limite.
func TestBackfillMaterializeRespectsLimit(t *testing.T) {
	ts, pool := setupOnDemand(t)
	_, owner := createProducer(t, ts, "Casa Backfill", "owner@backfill.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	_ = buyStanding(t, ts, pool, owner, 3)

	got, err := chain.BackfillMaterialize(ctx, pool, pid, chain.BackfillFilter{Limit: 2})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if got != 2 {
		t.Fatalf("backfill: esperava 2 materializados, veio %d", got)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM chain_jobs WHERE reason='backfill'`); n != 1 {
		t.Fatalf("esperava 1 job de backfill (pista agrupada), veio %d", n)
	}
	// Segunda execução pega o restante.
	got2, err := chain.BackfillMaterialize(ctx, pool, pid, chain.BackfillFilter{Limit: 100})
	if err != nil {
		t.Fatalf("backfill 2: %v", err)
	}
	if got2 != 1 {
		t.Fatalf("backfill 2: esperava 1 restante, veio %d", got2)
	}
}

// TestEagerReproducesOldBehavior: CHAIN_MINT_MODE=eager enfileira o mint no pagamento.
func TestEagerReproducesOldBehavior(t *testing.T) {
	ts, pool, _ := setupCore(t, okChain{}) // eager (default do harness)
	_, owner := createProducer(t, ts, "Casa Eager", "owner@eager.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	_ = buyStanding(t, ts, pool, owner, 1)
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM chain_jobs WHERE kind='mint'`); n != 1 {
		t.Fatalf("eager: esperava 1 job de mint no pagamento, veio %d", n)
	}
	if st := scanStr(t, ctx, pool, pid, `SELECT chain_status FROM tickets LIMIT 1`); st != "pending" {
		t.Fatalf("eager: esperava pending, veio %s", st)
	}
}

// TestFreeTransferDoesNotCreateChainJobs: transferência gratuita não cria job.
func TestFreeTransferDoesNotCreateChainJobs(t *testing.T) {
	ts, pool := setupOnDemand(t)
	_, owner := createProducer(t, ts, "Casa Free", "owner@free.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	_ = buyStanding(t, ts, pool, owner, 1)
	tid := firstTicket(t, ctx, pool, pid)
	ana := verifyOTP(t, ts, pool, "buy@ondemand.com", "123456")

	if code, _ := do(t, ts, "POST", "/api/v1/public/me/tickets/"+tid.String()+"/transfer", bearer(ana),
		map[string]any{"to_email": "bruno@free.com"}); code != http.StatusOK {
		t.Fatalf("transferência gratuita: %d", code)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM chain_jobs`); n != 0 {
		t.Fatalf("transferência gratuita não deveria criar chain_jobs, veio %d", n)
	}
}

// materialize roda chain.Materialize dentro de uma transação escopada ao tenant.
func materialize(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid uuid.UUID, ids []uuid.UUID, reason string) {
	t.Helper()
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := chain.Materialize(ctx, tx, ids, reason); err != nil {
			t.Fatalf("materialize: %v", err)
		}
	})
}
