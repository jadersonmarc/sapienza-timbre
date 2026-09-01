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
