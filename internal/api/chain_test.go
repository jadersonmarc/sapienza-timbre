package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-timbre/internal/chain"
	"github.com/jadersonmarc/sapienza-timbre/internal/ledger"
	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
)

// okChain minta com sucesso; failChain simula RPC fora do ar.
type okChain struct{}

func (okChain) Enabled() bool { return true }
func (okChain) Mint(_ context.Context, req chain.MintRequest) (chain.MintResult, error) {
	return chain.MintResult{TxHash: "0xok", TokenID: req.TokenID}, nil
}
func (okChain) Transfer(context.Context, chain.TransferRequest) (chain.TransferResult, error) {
	return chain.TransferResult{}, nil
}
func (okChain) Burn(context.Context, string) error { return nil }
func (okChain) Status(context.Context, string) (chain.TokenStatus, error) {
	return chain.TokenStatus{}, nil
}

type failChain struct{}

func (failChain) Enabled() bool { return true }
func (failChain) Mint(context.Context, chain.MintRequest) (chain.MintResult, error) {
	return chain.MintResult{}, errors.New("RPC down")
}
func (failChain) Transfer(context.Context, chain.TransferRequest) (chain.TransferResult, error) {
	return chain.TransferResult{}, errors.New("RPC down")
}
func (failChain) Burn(context.Context, string) error { return errors.New("RPC down") }
func (failChain) Status(context.Context, string) (chain.TokenStatus, error) {
	return chain.TokenStatus{}, errors.New("RPC down")
}

func scanStr(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid uuid.UUID, sql string, args ...any) string {
	t.Helper()
	var v string
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
	})
	return v
}

func soldStandingTicket(t *testing.T, ts *httptest.Server, owner string) string {
	t.Helper()
	eventID := createEvent(t, ts, owner, "Show Chain", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	_, lots := getEventLots(t, ts, owner, eventID)
	_, body := do(t, ts, "POST", "/api/v1/public/checkout", nil, map[string]any{
		"event_id": eventID, "lot_id": lots[0], "quantity": 1, "method": "pix",
	})
	ref, _ := body["asaas_ref"].(string)
	confirmWebhook(t, ts, ref)
	return eventID
}

// TestChainMintAsync: a venda gera o ingresso com chain_status=pending e o mint
// acontece em segundo plano (worker), virando minted.
func TestChainMintAsync(t *testing.T) {
	ts, pool, _ := setupCore(t, okChain{})
	_, owner := createProducer(t, ts, "Casa Chain", "owner@chain.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	soldStandingTicket(t, ts, owner)

	// Na venda: pendente + job na fila.
	if st := scanStr(t, ctx, pool, pid, `SELECT chain_status FROM tickets LIMIT 1`); st != "pending" {
		t.Fatalf("esperava chain_status pending, veio %s", st)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM chain_jobs WHERE status='pending'`); n != 1 {
		t.Fatalf("esperava 1 job pendente, veio %d", n)
	}

	// Worker minta em segundo plano.
	minted, err := chain.ProcessTenant(ctx, pool, okChain{}, pid)
	if err != nil || minted != 1 {
		t.Fatalf("process: minted=%d err=%v", minted, err)
	}
	if st := scanStr(t, ctx, pool, pid, `SELECT chain_status FROM tickets LIMIT 1`); st != "minted" {
		t.Fatalf("esperava minted, veio %s", st)
	}
	if tok := scanStr(t, ctx, pool, pid, `SELECT COALESCE(chain_token_id,'') FROM tickets LIMIT 1`); tok == "" {
		t.Fatal("esperava chain_token_id preenchido")
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM chain_jobs WHERE status='done'`); n != 1 {
		t.Fatalf("esperava 1 job done, veio %d", n)
	}
}

// TestChainDownDoesNotBlock é o "pronto quando" da Etapa 1.8: com o RPC fora do ar, a
// venda e a ENTRADA na portaria funcionam; o ingresso segue válido e o job re-tenta.
func TestChainDownDoesNotBlock(t *testing.T) {
	ts, pool, _ := setupCore(t, failChain{})
	_, owner := createProducer(t, ts, "Casa RPC", "owner@rpc.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()

	// Venda com mapa (para validar entrada por assento).
	eventID, seats, lotID := seatedEvent(t, ts, pool, owner, pid, 1)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	_, body := do(t, ts, "POST", "/api/v1/public/checkout", nil, map[string]any{
		"event_id": eventID.String(), "lot_id": lotID.String(), "quantity": 1,
		"seat_ids": []string{seats[0].String()}, "method": "pix",
	})
	confirmWebhook(t, ts, body["asaas_ref"].(string))

	// A venda concluiu: ingresso ativo e pendente na rede (não bloqueou).
	if st := scanStr(t, ctx, pool, pid, `SELECT status FROM tickets LIMIT 1`); st != "active" {
		t.Fatalf("esperava ingresso active, veio %s", st)
	}
	if st := scanStr(t, ctx, pool, pid, `SELECT chain_status FROM tickets LIMIT 1`); st != "pending" {
		t.Fatalf("esperava chain_status pending, veio %s", st)
	}

	// A ENTRADA funciona mesmo com a rede fora.
	var token string
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		var tid uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM tickets LIMIT 1`).Scan(&tid); err != nil {
			t.Fatalf("ticket: %v", err)
		}
		var e error
		token, e = ticketing.TicketToken(ctx, tx, tid)
		if e != nil {
			t.Fatalf("token: %v", e)
		}
	})
	if _, vb := do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner), map[string]any{"token": token, "gate": "G1"}); vb["verdict"] != "admitted" {
		t.Fatalf("entrada com RPC fora: esperava admitted, veio %v", vb)
	}

	// O worker tenta e falha: job volta a pending (retry), ingresso segue válido.
	minted, err := chain.ProcessTenant(ctx, pool, failChain{}, pid)
	if err != nil || minted != 0 {
		t.Fatalf("process: minted=%d err=%v", minted, err)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM chain_jobs WHERE status='pending' AND attempts=1`); n != 1 {
		t.Fatalf("esperava 1 job re-tentando (attempts=1), veio %d", n)
	}
	if st := scanStr(t, ctx, pool, pid, `SELECT status FROM tickets LIMIT 1`); st != "active" {
		t.Fatalf("ingresso deveria seguir válido, veio %s", st)
	}
}

// TestPayoutSettlement: o repasse disponível vira um payout.
func TestPayoutSettlement(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Payout", "owner@payout.com", "senha1234")
	pid := producerID(t, ts, owner)
	setRetention(t, pool, pid, 10)
	ctx := context.Background()
	soldStandingTicket(t, ts, owner) // 5000, retenção 10% → repasse 4500

	// O repasse nasce disponível D+2 (futuro); antecipamos para o passado no teste.
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `UPDATE ledger_entries SET available_at = now() - interval '1 day' WHERE kind='repasse'`); err != nil {
			t.Fatalf("ajustar available_at: %v", err)
		}
	})

	var amount int64
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		var e error
		if amount, e = ledger.SettleDue(ctx, tx); e != nil {
			t.Fatalf("settle: %v", e)
		}
	})
	if amount != 4500 {
		t.Fatalf("esperava payout 4500, veio %d", amount)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM payouts WHERE amount_cents=4500 AND status='pending'`); n != 1 {
		t.Fatalf("esperava 1 payout de 4500, veio %d", n)
	}
	// Idempotente: já pago → nada novo.
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		amt, _ := ledger.SettleDue(ctx, tx)
		if amt != 0 {
			t.Fatalf("segundo settle deveria ser 0, veio %d", amt)
		}
	})
}
