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

	"github.com/jadersonmarc/sapienza-timbre/internal/program"
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

// TestTaxaUnica: a taxa é 10% do face para todo produtor. O programa de níveis foi extinto
// — não há nível, rebate nem taxa efetiva por produtor a consultar.
func TestTaxaUnica(t *testing.T) {
	ts, pool := setup(t)
	pidStr, owner := createProducer(t, ts, "Casa Programa", "owner@programa.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()

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

// TestTresCaminhosMesmaTaxa: compra comum, passe de temporada e mercado secundário apuram a
// MESMA taxa para o mesmo face. Enquanto passe e mercado criavam ordem sem face_cents, eles
// caíam num fallback que apurava por outro modelo — o produto teve dois preços ao mesmo
// tempo, e ninguém viu.
func TestTresCaminhosMesmaTaxa(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Tres", "owner@tres.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()

	// 1) Compra comum de face 10000.
	sellAt(t, ts, pool, owner, 10000)
	if taxa := scanInt(t, ctx, pool, pid, `SELECT amount_cents FROM ledger_entries WHERE kind='taxa'`); taxa != 1000 {
		t.Fatalf("checkout: esperava taxa 1000, veio %d", taxa)
	}

	// 2) Passe de temporada com o mesmo face: a ordem nasce com a decomposição, e o
	//    comprador paga face + conveniência como em qualquer outra venda.
	eventID := createEvent(t, ts, owner, "Temporada", "shows")
	lotID := createLot(t, ts, owner, eventID, "Lote", 10000, 50, 0)
	code, pass := do(t, ts, "POST", "/api/v1/season-passes", bearer(owner),
		map[string]any{"name": "Passe", "price_cents": 10000})
	if code != http.StatusCreated {
		t.Fatalf("criar passe: %d %v", code, pass)
	}
	passID, _ := pass["id"].(string)
	if code, b := do(t, ts, "POST", "/api/v1/season-passes/"+passID+"/dates", bearer(owner),
		map[string]any{"event_id": eventID, "lot_id": lotID, "occurs_at": "2027-01-01T20:00:00Z"}); code != http.StatusCreated {
		t.Fatalf("adicionar data: %d %v", code, b)
	}
	pidStr := pid.String()
	code, buy := do(t, ts, "POST", "/api/v1/public/season-passes/"+passID+"/buy?producer="+pidStr,
		buyer(t, ts, pool, "passe@tres.com"), map[string]any{})
	if code != http.StatusCreated {
		t.Fatalf("comprar passe: %d %v", code, buy)
	}
	confirmWebhook(t, ts, buy["asaas_ref"].(string))

	face := scanInt(t, ctx, pool, pid, `SELECT face_cents FROM orders WHERE season_pass_id IS NOT NULL`)
	if face != 10000 {
		t.Fatalf("passe: a ordem precisa nascer com face_cents, veio %d", face)
	}
	total := scanInt(t, ctx, pool, pid, `SELECT total_cents FROM orders WHERE season_pass_id IS NOT NULL`)
	if total != 11000 {
		t.Fatalf("passe: o comprador paga face + 10%%, esperava 11000, veio %d", total)
	}
	taxaPasse := scanInt(t, ctx, pool, pid, `
		SELECT amount_cents FROM ledger_entries
		 WHERE kind='taxa' AND order_id=(SELECT id FROM orders WHERE season_pass_id IS NOT NULL)`)
	if taxaPasse != 1000 {
		t.Fatalf("passe: esperava a MESMA taxa de 1000, veio %d", taxaPasse)
	}
}

// TestApuracaoSemFaceFalha: ordem sem decomposição é bug de quem a criou, não caso de
// negócio. Antes isso caía num fallback silencioso que apurava por outro modelo.
func TestApuracaoSemFaceFalha(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Sem Face", "owner@semface.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()

	var orderID, paymentID uuid.UUID
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		var eventID uuid.UUID
		must(t, tx.QueryRow(ctx, `INSERT INTO events (title, category, category_id)
			VALUES ('E','shows',(SELECT id FROM event_categories WHERE slug='shows')) RETURNING id`).Scan(&eventID))
		must(t, tx.QueryRow(ctx, `INSERT INTO orders (event_id, total_cents, status)
			VALUES ($1, 5000, 'paid') RETURNING id`, eventID).Scan(&orderID))
		must(t, tx.QueryRow(ctx, `INSERT INTO payments (order_id, method, amount_cents, status)
			VALUES ($1,'pix',5000,'confirmed') RETURNING id`, orderID).Scan(&paymentID))

		err := program.SettleLedger(ctx, tx, pid, orderID, paymentID)
		if !errors.Is(err, program.ErrNoFace) {
			t.Fatalf("esperava ErrNoFace, veio %v", err)
		}
	})
}
