package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestWebhookIdempotentByEventID: o gateway reenvia o mesmo aviso. A deduplicação é pelo id
// DO EVENTO, não pela cobrança — uma mesma cobrança gera confirmação e estorno, e deduplicar
// por cobrança descartaria avisos legítimos.
func TestWebhookIdempotentByEventID(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Idem", "owner@idem.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Idem", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 10000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar")
	}
	body := buyViaSession(t, ts, buyer(t, ts, pool, "idem@x.com"),
		map[string]any{"event_id": eventID, "quantity": 2}, "pix")
	ref := body["asaas_ref"].(string)

	// Id único por execução (o banco de teste é compartilhado), mas o MESMO nas três
	// entregas — é isso que o gateway faz ao reenviar.
	evento := map[string]any{
		"id": "evt_" + uuid.NewString(), "asaas_ref": ref, "confirmed": true, "type": "PAYMENT_CONFIRMED",
	}
	for i := 0; i < 3; i++ {
		if code, _ := do(t, ts, "POST", "/api/v1/webhooks/asaas", nil, evento); code != http.StatusOK {
			t.Fatalf("webhook %d", i)
		}
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets`); n != 2 {
		t.Fatalf("o mesmo evento reenviado deveria emitir 2 ingressos, veio %d", n)
	}
}
