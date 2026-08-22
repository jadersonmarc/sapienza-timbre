package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
)

// TestEmissionSignsTicketOffline: um ingresso emitido na compra é assinado, e o QR é
// validado por um verificador que só conhece a chave pública — sem tocar no banco.
func TestEmissionSignsTicketOffline(t *testing.T) {
	ts, pool, signer := setupSigned(t)
	_, owner := createProducer(t, ts, "Casa QR", "owner@qr.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()

	eventID := createEvent(t, ts, owner, "Show QR", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	_, _ = getEventLots(t, ts, owner, eventID)

	body := buyViaSession(t, ts, buyer(t, ts, pool, "c@ticket.com"), map[string]any{
		"event_id": eventID, "quantity": 1,
	}, "pix")
	asaasRef, _ := body["asaas_ref"].(string)
	confirmWebhook(t, ts, asaasRef)

	// Ingresso assinado (signature + qr_nonce preenchidos) e token reconstruível.
	var ticketID uuid.UUID
	var token string
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT id FROM tickets WHERE signature IS NOT NULL AND qr_nonce IS NOT NULL LIMIT 1`).Scan(&ticketID); err != nil {
			t.Fatalf("ingresso assinado não encontrado: %v", err)
		}
		var e error
		if token, e = ticketing.TicketToken(ctx, tx, ticketID); e != nil {
			t.Fatalf("token: %v", e)
		}
	})

	// Validação OFFLINE: só a chave pública, sem banco.
	v, err := ticketing.NewVerifier(signer.PublicKeyB64())
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	payload, err := v.Verify(token)
	if err != nil {
		t.Fatalf("verify offline: %v", err)
	}
	if payload.TicketID != ticketID {
		t.Fatalf("payload.TicketID %s != %s", payload.TicketID, ticketID)
	}
}

// TestRefundBurnsTickets: uma contestação queima os ingressos e libera os assentos.
func TestRefundBurnsTickets(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Estorno", "owner@estorno.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, seats, _ := seatedEvent(t, ts, pool, owner, pid, 2)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}

	body := buyViaSession(t, ts, buyer(t, ts, pool, "buy@refund.com"), map[string]any{
		"event_id": eventID.String(), "quantity": 2,
		"seat_ids": []string{seats[0].String(), seats[1].String()},
	}, "pix")
	asaasRef, _ := body["asaas_ref"].(string)
	confirmWebhook(t, ts, asaasRef)

	// Contestação/estorno.
	code, _ := do(t, ts, "POST", "/api/v1/webhooks/asaas", nil,
		map[string]any{"asaas_ref": asaasRef, "refunded": true, "type": "PAYMENT_REFUNDED"})
	if code != http.StatusOK {
		t.Fatalf("webhook refund: %d", code)
	}

	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='burned'`); n != 2 {
		t.Fatalf("esperava 2 ingressos queimados, veio %d", n)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM orders WHERE status='refunded'`); n != 1 {
		t.Fatalf("esperava 1 ordem estornada, veio %d", n)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM seat_occupancy WHERE NOT released`); n != 0 {
		t.Fatalf("esperava 0 ocupações vivas após estorno, veio %d", n)
	}
	// Assentos liberados: dá pra segurar de novo.
	if _, err := holdTx(ctx, pool, pid, eventID, seats, time.Minute); err != nil {
		t.Fatalf("re-hold após estorno: %v", err)
	}
}
