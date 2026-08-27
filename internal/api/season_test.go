package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestSeasonPassIssuesPerDate é o "pronto quando" da Etapa 2.3: a compra de um passe
// emite um ingresso por data, e cada ingresso é independente (transferível sozinho).
func TestSeasonPassIssuesPerDate(t *testing.T) {
	ts, pool := setup(t)
	pidStr, owner := createProducer(t, ts, "Casa Temporada", "owner@temporada.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()

	eventID := createEvent(t, ts, owner, "Festival", "festas")
	lotID := createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)

	// Cria o passe (8000) com 2 datas (2 noites do mesmo evento/lote).
	_, pbody := do(t, ts, "POST", "/api/v1/season-passes", bearer(owner),
		map[string]any{"name": "Passe 2 noites", "price_cents": 8000})
	passID, _ := pbody["id"].(string)
	for _, occ := range []string{"2026-09-01T20:00:00Z", "2026-09-02T20:00:00Z"} {
		if code, _ := do(t, ts, "POST", "/api/v1/season-passes/"+passID+"/dates", bearer(owner),
			map[string]any{"event_id": eventID, "lot_id": lotID, "occurs_at": occ}); code != http.StatusCreated {
			t.Fatalf("adicionar data: %d", code)
		}
	}

	// Compra do passe pelo comprador autenticado.
	code, bbody := do(t, ts, "POST", "/api/v1/public/season-passes/"+passID+"/buy?producer="+pidStr, buyer(t, ts, pool, "passe@x.com"),
		map[string]any{})
	if code != http.StatusCreated {
		t.Fatalf("comprar passe: %d %v", code, bbody)
	}
	confirmWebhook(t, ts, bbody["asaas_ref"].(string))

	// Dois ingressos emitidos, vinculados ao passe.
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE season_pass_id=$1 AND status='active'`, uuid.MustParse(passID)); n != 2 {
		t.Fatalf("esperava 2 ingressos do passe, veio %d", n)
	}
	// Taxa do passe: 10% do face, a MESMA de qualquer outro caminho de venda.
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM ledger_entries WHERE kind='taxa' AND amount_cents=800`); n != 1 {
		t.Fatalf("esperava taxa 800 (10%% de 8000), veio %d", n)
	}

	// Cada ingresso é destacável/transferível individualmente.
	var ids []uuid.UUID
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		rows, err := tx.Query(ctx, `SELECT id FROM tickets WHERE season_pass_id=$1 ORDER BY created_at`, uuid.MustParse(passID))
		if err != nil {
			t.Fatalf("tickets: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			_ = rows.Scan(&id)
			ids = append(ids, id)
		}
	})
	if len(ids) != 2 {
		t.Fatalf("esperava 2 ids, veio %d", len(ids))
	}
	wallet := mkWallet(t, pool, "0xpasse-dono")
	if code, _ := do(t, ts, "POST", "/api/v1/tickets/"+ids[0].String()+"/transfer", bearer(owner),
		map[string]any{"to_wallet_id": wallet.String(), "price_cents": 3000}); code != http.StatusCreated {
		t.Fatalf("transferir 1 ingresso do passe: %d", code)
	}
	// Só o primeiro mudou de dono.
	if o := scanStr(t, ctx, pool, pid, `SELECT COALESCE(owner_wallet_id::text,'') FROM tickets WHERE id=$1`, ids[0]); o != wallet.String() {
		t.Fatalf("1º ingresso deveria ter novo dono, veio %s", o)
	}
	if o := scanStr(t, ctx, pool, pid, `SELECT COALESCE(owner_wallet_id::text,'') FROM tickets WHERE id=$1`, ids[1]); o != "" {
		t.Fatalf("2º ingresso NÃO deveria ter mudado, veio %s", o)
	}
}
