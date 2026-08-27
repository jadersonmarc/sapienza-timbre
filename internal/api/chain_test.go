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

func soldStandingTicket(t *testing.T, ts *httptest.Server, pool *pgxpool.Pool, owner string) string {
	t.Helper()
	eventID := createEvent(t, ts, owner, "Show Chain", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	_, _ = getEventLots(t, ts, owner, eventID)
	body := buyViaSession(t, ts, buyer(t, ts, pool, "buy@chain.com"), map[string]any{
		"event_id": eventID, "quantity": 1,
	}, "pix")
	ref, _ := body["asaas_ref"].(string)
	confirmWebhook(t, ts, ref)
	return eventID
}

// TestPayoutSettlement: o repasse vira payout SÓ quando o dinheiro está com a plataforma.
//
// Venda liquidada por split não gera payout: o gateway entregou o face direto na subconta
// do produtor, na própria cobrança. Materializar payout também para ela mandaria transferir
// de novo o que já foi pago — o razão contava a mesma venda duas vezes.
func TestPayoutSettlement(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Payout", "owner@payout.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	soldStandingTicket(t, ts, pool, owner) // face 5000, liquidado por SPLIT

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
	if amount != 0 {
		t.Fatalf("venda com split não deveria gerar payout, veio %d", amount)
	}

	// Venda CENTRALIZADA (sem split): o dinheiro está com a plataforma e vira payout.
	seedCardSale(t, ctx, pool, pid)
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		// Libera a retenção de 60 dias para o líquido ficar cheio.
		if _, err := tx.Exec(ctx, `UPDATE ledger_entries SET available_at = now() - interval '1 day' WHERE kind='retencao'`); err != nil {
			t.Fatalf("liberar retenção: %v", err)
		}
	})
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		var e error
		if amount, e = ledger.SettleDue(ctx, tx); e != nil {
			t.Fatalf("settle centralizado: %v", e)
		}
	})
	if amount != 5000 {
		t.Fatalf("esperava payout 5000, veio %d", amount)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM payouts WHERE amount_cents=5000 AND status='pending'`); n != 1 {
		t.Fatalf("esperava 1 payout de 5000, veio %d", n)
	}
	// Idempotente: já pago → nada novo.
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		amt, _ := ledger.SettleDue(ctx, tx)
		if amt != 0 {
			t.Fatalf("segundo settle deveria ser 0, veio %d", amt)
		}
	})
}
