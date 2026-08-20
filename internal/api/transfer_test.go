package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-timbre/internal/chain"
)

// mkWallet cria um subject + wallet em public e devolve o id da carteira.
func mkWallet(t *testing.T, pool *pgxpool.Pool, address string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var sid, wid uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO subjects DEFAULT VALUES RETURNING id`).Scan(&sid); err != nil {
		t.Fatalf("subject: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO wallets (subject_id, address) VALUES ($1,$2) RETURNING id`, sid, address).Scan(&wid); err != nil {
		t.Fatalf("wallet: %v", err)
	}
	return wid
}

func firstTicket(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT id FROM tickets LIMIT 1`).Scan(&id); err != nil {
			t.Fatalf("ticket: %v", err)
		}
	})
	return id
}

// TestRestrictedTransfer cobre o teto, o royalty, a reatribuição de dono e o registro
// on-chain assíncrono da transferência.
func TestRestrictedTransfer(t *testing.T) {
	ts, pool, _ := setupCore(t, okChain{})
	_, owner := createProducer(t, ts, "Casa Transf", "owner@transf.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	soldStandingTicket(t, ts, pool, owner) // Pix: transferível imediatamente; face 5000
	tid := firstTicket(t, ctx, pool, pid)
	wallet := mkWallet(t, pool, "0xnovo-dono-1")

	// Acima do teto (face 5000, cap 100%) → recusa.
	if code, _ := do(t, ts, "POST", "/api/v1/tickets/"+tid.String()+"/transfer", bearer(owner),
		map[string]any{"to_wallet_id": wallet.String(), "price_cents": 6000}); code != http.StatusBadRequest {
		t.Fatalf("acima do teto: esperava 400, veio %d", code)
	}

	// Dentro do teto → sucesso, royalty 10%.
	code, body := do(t, ts, "POST", "/api/v1/tickets/"+tid.String()+"/transfer", bearer(owner),
		map[string]any{"to_wallet_id": wallet.String(), "price_cents": 4000})
	if code != http.StatusCreated {
		t.Fatalf("transfer: %d %v", code, body)
	}
	if body["royalty_cents"].(float64) != 400 {
		t.Fatalf("royalty esperado 400, veio %v", body["royalty_cents"])
	}

	// Dono reatribuído + registros.
	if got := scanStr(t, ctx, pool, pid, `SELECT owner_wallet_id::text FROM tickets WHERE id=$1`, tid); got != wallet.String() {
		t.Fatalf("dono não reatribuído: %s", got)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM transfers WHERE ticket_id=$1`, tid); n != 1 {
		t.Fatalf("esperava 1 transfer, veio %d", n)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM royalty_entries WHERE amount_cents=400`); n != 1 {
		t.Fatalf("esperava 1 royalty_entry de 400, veio %d", n)
	}

	// Registro on-chain assíncrono confirma o transfer.
	if _, err := chain.ProcessTenant(ctx, pool, okChain{}, pid); err != nil {
		t.Fatalf("process transfer: %v", err)
	}
	if st := scanStr(t, ctx, pool, pid, `SELECT status FROM transfers WHERE ticket_id=$1`, tid); st != "confirmed" {
		t.Fatalf("transfer on-chain: esperava confirmed, veio %s", st)
	}
}

// TestTransferBlockedInWindow: dentro da janela de contestação (cartão, +60d) o
// ingresso não é transferível.
func TestTransferBlockedInWindow(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Janela", "owner@janela.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()

	eventID := createEvent(t, ts, owner, "Show Janela", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil)
	_, lots := getEventLots(t, ts, owner, eventID)
	// Cartão → transferable_after = now + 60d.
	_, body := do(t, ts, "POST", "/api/v1/public/checkout", buyer(t, ts, pool, "buy@transfer.com"), map[string]any{
		"event_id": eventID, "lot_id": lots[0], "quantity": 1, "method": "credit_card",
	})
	confirmWebhook(t, ts, body["asaas_ref"].(string))

	tid := firstTicket(t, ctx, pool, pid)
	wallet := mkWallet(t, pool, "0xnovo-dono-2")
	if code, _ := do(t, ts, "POST", "/api/v1/tickets/"+tid.String()+"/transfer", bearer(owner),
		map[string]any{"to_wallet_id": wallet.String(), "price_cents": 1000}); code != http.StatusConflict {
		t.Fatalf("dentro da janela: esperava 409, veio %d", code)
	}
}
