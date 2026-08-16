package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// sellAt vende 1 ingresso de um evento com lote de `price` e devolve o event id.
func sellAt(t *testing.T, ts *httptest.Server, owner string, price int64) string {
	t.Helper()
	eventID := createEvent(t, ts, owner, "Prog", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote", price, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	_, lots := getEventLots(t, ts, owner, eventID)
	_, body := do(t, ts, "POST", "/api/v1/public/checkout", nil, map[string]any{
		"event_id": eventID, "lot_id": lots[0], "quantity": 1, "method": "pix",
	})
	confirmWebhook(t, ts, body["asaas_ref"].(string))
	return eventID
}

// TestProducerProgram cobre a apuração CORRIGIDA: taxa 15%, níveis Iniciante 10% /
// Pro 15% / Sênior 20% (rebate provisório da taxa), pelo nível vigente na venda.
func TestProducerProgram(t *testing.T) {
	ts, pool := setup(t)
	pidStr, owner := createProducer(t, ts, "Casa Programa", "owner@programa.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()

	// Default = iniciante: taxa 15%, nível 10%.
	code, body := do(t, ts, "GET", "/api/v1/dash/program", bearer(owner), nil)
	if code != http.StatusOK || body["tier"] != "iniciante" || body["fee_pct"].(float64) != 15 || body["tier_pct"].(float64) != 10 {
		t.Fatalf("programa inicial: %d %v", code, body)
	}

	// Transição para sênior.
	setTier(t, ts, pidStr, "senior")
	_, body = do(t, ts, "GET", "/api/v1/dash/program", bearer(owner), nil)
	if body["tier"] != "senior" || body["tier_pct"].(float64) != 20 {
		t.Fatalf("após transição: %v", body)
	}

	// Venda de face 10000 como sênior (modelo Sympla §4): taxa = 10% (1000) − rebate 20% (200) = 800.
	sellAt(t, ts, owner, 10000)
	if taxa := scanInt(t, ctx, pool, pid, `SELECT amount_cents FROM ledger_entries WHERE kind='taxa'`); taxa != 800 {
		t.Fatalf("taxa sênior: esperava 800, veio %d", taxa)
	}

	// Originação inerte: participação default 0 → nenhuma apuração de originador.
	origStr, _ := createProducer(t, ts, "Originador", "owner@orig.com", "senha1234")
	if code, _ := do(t, ts, "POST", "/api/v1/admin/producers/"+pidStr+"/origination",
		map[string]string{"X-Admin-Token": adminToken}, map[string]any{"originator_id": origStr}); code != http.StatusOK {
		t.Fatalf("set origination: %d", code)
	}
	sellAt(t, ts, owner, 5000)
	var origEntries int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM origination_entries`).Scan(&origEntries); err != nil {
		t.Fatalf("origination_entries: %v", err)
	}
	if origEntries != 0 {
		t.Fatalf("participação provisória 0 deveria manter originação inerte, veio %d", origEntries)
	}
}
