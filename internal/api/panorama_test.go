package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
)

func ticketOfEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid, eventID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT id FROM tickets WHERE event_id=$1 LIMIT 1`, eventID).Scan(&id); err != nil {
			t.Fatalf("ticket do evento: %v", err)
		}
	})
	return id
}

func tokenOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid, ticketID uuid.UUID) string {
	t.Helper()
	var tok string
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		var e error
		if tok, e = ticketing.TicketToken(ctx, tx, ticketID); e != nil {
			t.Fatalf("token: %v", e)
		}
	})
	return tok
}

// TestPanorama é o "pronto quando" da Etapa 2.5: quem passou por 2 lugares vê o mapa/
// linha do tempo e a retrospectiva do ano.
func TestPanorama(t *testing.T) {
	ts, pool, _ := setupSigned(t)
	_, owner := createProducer(t, ts, "Casa Panorama", "owner@panorama.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()

	ev1 := uuid.MustParse(soldStandingTicket(t, ts, owner))
	ev2 := uuid.MustParse(soldStandingTicket(t, ts, owner))
	tid1 := ticketOfEvent(t, ctx, pool, pid, ev1)
	tid2 := ticketOfEvent(t, ctx, pool, pid, ev2)

	// Uma carteira/sujeito recebe os dois ingressos.
	wallet := mkWallet(t, pool, "0xpanorama")
	var subjectID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT subject_id FROM wallets WHERE id=$1`, wallet).Scan(&subjectID); err != nil {
		t.Fatalf("subject: %v", err)
	}
	for _, tid := range []uuid.UUID{tid1, tid2} {
		if code, _ := do(t, ts, "POST", "/api/v1/tickets/"+tid.String()+"/transfer", bearer(owner),
			map[string]any{"to_wallet_id": wallet.String(), "price_cents": 0}); code != http.StatusCreated {
			t.Fatalf("transferir p/ o sujeito: %d", code)
		}
	}

	// Check-in nos dois → duas presenças do sujeito.
	for _, tid := range []uuid.UUID{tid1, tid2} {
		tok := tokenOf(t, ctx, pool, pid, tid)
		if _, vb := do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner), map[string]any{"token": tok, "gate": "G1"}); vb["verdict"] != "admitted" {
			t.Fatalf("admissão: %v", vb)
		}
	}

	// Panorama público (peça compartilhável).
	code, body := do(t, ts, "GET", "/api/v1/public/subjects/"+subjectID.String()+"/panorama", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("panorama: %d", code)
	}
	places, _ := body["places"].([]any)
	if len(places) != 2 {
		t.Fatalf("esperava 2 lugares, veio %v", body["places"])
	}
	p0, _ := places[0].(map[string]any)
	if p0["event_title"] == "" || p0["producer"] != "Casa Panorama" {
		t.Fatalf("lugar sem título/casa: %v", p0)
	}
	retro, _ := body["retrospective"].(map[string]any)
	if retro["events"].(float64) != 2 || retro["casas"].(float64) != 1 {
		t.Fatalf("retrospectiva: esperava events 2 / casas 1, veio %v", retro)
	}
}
