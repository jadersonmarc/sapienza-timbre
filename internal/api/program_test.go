package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sellAt vende 1 ingresso de um evento com lote de `price` e devolve o event id.
func sellAt(t *testing.T, ts *httptest.Server, pool *pgxpool.Pool, owner string, price int64) string {
	t.Helper()
	eventID := createEvent(t, ts, owner, "Prog", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote", price, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	_, _ = getEventLots(t, ts, owner, eventID)
	body := buyViaSession(t, ts, buyer(t, ts, pool, "buy@prog.com"), map[string]any{
		"event_id": eventID, "quantity": 1,
	}, "pix")
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
	setTier(t, ts, pool, pidStr, "senior")
	_, body = do(t, ts, "GET", "/api/v1/dash/program", bearer(owner), nil)
	if body["tier"] != "senior" || body["tier_pct"].(float64) != 20 {
		t.Fatalf("após transição: %v", body)
	}

	// O nível não mexe mais no preço: a taxa de plataforma é 10% do face para todo
	// produtor. O programa de produtores foi extinto; o nível fica só como registro.
	sellAt(t, ts, pool, owner, 10000)
	if taxa := scanInt(t, ctx, pool, pid, `SELECT amount_cents FROM ledger_entries WHERE kind='taxa'`); taxa != 1000 {
		t.Fatalf("taxa: esperava 10%% do face (1000), veio %d", taxa)
	}

	// Originação inerte: participação default 0 → nenhuma apuração de originador.
	origStr, _ := createProducer(t, ts, "Originador", "owner@orig.com", "senha1234")
	admin := seedAdmin(t, ts, pool, "admin@programa.com", "super_admin")
	if code, _ := do(t, ts, "POST", "/api/v1/admin/producers/"+pidStr+"/origination",
		admin, map[string]any{"originator_id": origStr}); code != http.StatusOK {
		t.Fatalf("set origination: %d", code)
	}
	sellAt(t, ts, pool, owner, 5000)
	var origEntries int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM origination_entries`).Scan(&origEntries); err != nil {
		t.Fatalf("origination_entries: %v", err)
	}
	if origEntries != 0 {
		t.Fatalf("participação provisória 0 deveria manter originação inerte, veio %d", origEntries)
	}
}
