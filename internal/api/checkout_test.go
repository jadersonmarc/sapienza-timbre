package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// confirmWebhook simula a confirmação do Asaas (payload do FakeGateway).
func confirmWebhook(t *testing.T, ts *httptest.Server, asaasRef string) {
	t.Helper()
	code, _ := do(t, ts, "POST", "/api/v1/webhooks/asaas", nil,
		map[string]any{"asaas_ref": asaasRef, "confirmed": true, "type": "PAYMENT_CONFIRMED"})
	if code != http.StatusOK {
		t.Fatalf("webhook: esperava 200, veio %d", code)
	}
}

func scanInt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid uuid.UUID, sql string, args ...any) int {
	t.Helper()
	var v int
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
	})
	return v
}

// TestCheckoutPixStandingCycle: uma compra em Pix percorre o ciclo completo — cobrança
// INTEIRA na conta da bilheteria, razão lançado e obrigação de repasse acumulando.
func TestCheckoutPixStandingCycle(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Pix", "owner@pix.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()

	eventID := createEvent(t, ts, owner, "Show Pix", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	// Descobre o lote ativo.
	_, lots := getEventLots(t, ts, owner, eventID)
	lotID := lots[0]

	// Compra pela sessão (cria → bind → pay), com o comprador autenticado.
	body := buyViaSession(t, ts, buyer(t, ts, pool, "comprador@x.com"), map[string]any{
		"event_id": eventID, "quantity": 2,
	}, "pix")
	if body["pix_code"] == nil || body["pix_code"] == "" {
		t.Fatalf("esperava pix_code no checkout, veio %v", body)
	}
	// Face limpo ao produtor: o comprador paga V = (face × 1,10 + b) / (1 − a), com a/b da
	// tabela de tarifas do gateway. Pix da tabela de teste: a = 0,99%, b = 0 →
	// (10000 × 1,10) / 0,9901 = 11110 (arredondado para cima).
	if amt := body["amount_cents"].(float64); amt != 11110 {
		t.Fatalf("esperava amount 11110 (face 10000 + conveniência 1110), veio %v", amt)
	}
	asaasRef, _ := body["asaas_ref"].(string)

	// Antes da confirmação: sem ingressos, ordem pendente.
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets`); n != 0 {
		t.Fatalf("antes do webhook esperava 0 ingressos, veio %d", n)
	}

	// Webhook confirma.
	confirmWebhook(t, ts, asaasRef)

	// Ordem paga, 2 ingressos ativos, estoque debitado.
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM orders WHERE status='paid'`); n != 1 {
		t.Fatalf("esperava 1 ordem paga, veio %d", n)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='active'`); n != 2 {
		t.Fatalf("esperava 2 ingressos ativos, veio %d", n)
	}
	if sold := scanInt(t, ctx, pool, pid, `SELECT sold_count FROM lots WHERE id=$1`, uuid.MustParse(lotID)); sold != 2 {
		t.Fatalf("esperava sold_count=2, veio %d", sold)
	}

	// Razão: repasse = FACE (10000, limpo ao produtor); taxa = 10% do face, igual para
	// todo produtor.
	if taxa := scanInt(t, ctx, pool, pid, `SELECT amount_cents FROM ledger_entries WHERE kind='taxa'`); taxa != 1000 {
		t.Fatalf("esperava taxa 1000 (10%% de 10000), veio %d", taxa)
	}
	if repasse := scanInt(t, ctx, pool, pid, `SELECT amount_cents FROM ledger_entries WHERE kind='repasse'`); repasse != 10000 {
		t.Fatalf("esperava repasse 10000 (face limpo), veio %d", repasse)
	}
	// A obrigação de repasse nasce ACUMULANDO: o evento ainda não aconteceu, e o valor fica
	// com a bilheteria até lá. O líquido do produtor é o face, sem a conveniência.
	if s := scanText(t, ctx, pool, pid, `SELECT status FROM event_payouts LIMIT 1`); s != "accruing" {
		t.Fatalf("esperava repasse acumulando antes do evento, veio %q", s)
	}
	if v := scanInt(t, ctx, pool, pid, `SELECT net_due_cents FROM event_payouts LIMIT 1`); v != 10000 {
		t.Fatalf("esperava líquido 10000 (o face), veio %d", v)
	}
	if v := scanInt(t, ctx, pool, pid, `SELECT platform_fee_cents FROM event_payouts LIMIT 1`); v != 1000 {
		t.Fatalf("esperava conveniência 1000 para a plataforma, veio %d", v)
	}

	// Idempotência: reenviar o webhook não duplica ingressos.
	confirmWebhook(t, ts, asaasRef)
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets`); n != 2 {
		t.Fatalf("após reenvio do webhook esperava 2 ingressos, veio %d", n)
	}
}

// TestCheckoutPixSeatedCycle: compra com mapa reserva por hold e emite ingressos com
// assento; os assentos ficam ocupados.
func TestCheckoutPixSeatedCycle(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Assento", "owner@assento.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, seats, _ := seatedEvent(t, ts, pool, owner, pid, 2)

	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}

	body := buyViaSession(t, ts, buyer(t, ts, pool, "buy@assento.com"), map[string]any{
		"event_id": eventID.String(), "quantity": 2,
		"seat_ids": []string{seats[0].String(), seats[1].String()},
	}, "pix")
	asaasRef, _ := body["asaas_ref"].(string)
	confirmWebhook(t, ts, asaasRef)

	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='active' AND seat_id IS NOT NULL`); n != 2 {
		t.Fatalf("esperava 2 ingressos com assento, veio %d", n)
	}
	// Assentos ocupados por ingresso: nova sessão com os mesmos assentos falha na reserva.
	code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil, map[string]any{
		"event_id": eventID.String(), "quantity": 1, "seat_ids": []string{seats[0].String()},
	})
	if code != http.StatusConflict {
		t.Fatalf("reserva de assento ocupado: esperava 409, veio %d", code)
	}
}

// TestCourtesyWithSeat: cortesia com assento ocupa-o; segunda cortesia no mesmo
// assento é recusada.
func TestCourtesyWithSeat(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Cortesia", "owner@cortesia.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, seats, lotID := seatedEvent(t, ts, pool, owner, pid, 2)
	catID := courtesyCategoryID(t, ctx, pool, pid, "convidado")

	code, body := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/guests", bearer(owner),
		map[string]any{"name": "Convidada", "cpf": "12345678900", "lot_id": lotID.String(), "seat_id": seats[0].String(), "courtesy_category_id": catID})
	if code != http.StatusCreated {
		t.Fatalf("cortesia: status %d, body %v", code, body)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE seat_id=$1 AND status='active'`, seats[0]); n != 1 {
		t.Fatalf("esperava 1 ingresso de cortesia no assento, veio %d", n)
	}
	// Segunda cortesia no mesmo assento → 409.
	code, _ = do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/guests", bearer(owner),
		map[string]any{"name": "Outro", "lot_id": lotID.String(), "seat_id": seats[0].String(), "courtesy_category_id": catID})
	if code != http.StatusConflict {
		t.Fatalf("cortesia em assento ocupado: esperava 409, veio %d", code)
	}
}

// getEventLots devolve (event, lotIDs) via GET /events/{id}.
func getEventLots(t *testing.T, ts *httptest.Server, token, eventID string) (map[string]any, []string) {
	t.Helper()
	code, body := do(t, ts, "GET", "/api/v1/events/"+eventID, bearer(token), nil)
	if code != http.StatusOK {
		t.Fatalf("get event: %d", code)
	}
	ev, _ := body["event"].(map[string]any)
	var lotIDs []string
	if raw, ok := body["lots"].([]any); ok {
		for _, l := range raw {
			m, _ := l.(map[string]any)
			if id, ok := m["id"].(string); ok {
				lotIDs = append(lotIDs, id)
			}
		}
	}
	return ev, lotIDs
}
