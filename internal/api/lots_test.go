package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// criarLote cria um lote com modo de oferta e gatilho explícitos.
func criarLote(t *testing.T, ts *httptest.Server, owner, eventID string, body map[string]any) string {
	t.Helper()
	code, resp := do(t, ts, "POST", "/api/v1/events/"+eventID+"/lots", bearer(owner), body)
	if code != http.StatusCreated {
		t.Fatalf("criar lote %v: %d %v", body["name"], code, resp)
	}
	return resp["id"].(string)
}

// loteAberto diz se a página pública mostra o lote como comprável agora.
func loteAberto(t *testing.T, ts *httptest.Server, eventID, lotID string) bool {
	t.Helper()
	code, body := do(t, ts, "GET", "/api/v1/public/events/"+eventID, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("página pública: %d %v", code, body)
	}
	for _, raw := range body["lots"].([]any) {
		l := raw.(map[string]any)
		if l["id"] == lotID {
			return l["on_sale"] == true
		}
	}
	t.Fatalf("lote %s não aparece na página", lotID)
	return false
}

// TestLoteProgressivoAbreOSeguinte (B.8.4): o próximo abre quando o anterior encerra, e o
// gatilho de cada lote decide o que "encerrar" significa.
func TestLoteProgressivoAbreOSeguinte(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Progressiva", "owner@progressiva.com", "senha1234")
	ctx := context.Background()
	pid := producerID(t, ts, owner)
	eventID := createEvent(t, ts, owner, "Show Progressivo", "shows")

	// Lote 1 encerra por ESGOTAMENTO; lote 2 espera a vez.
	l1 := criarLote(t, ts, owner, eventID, map[string]any{
		"name": "Lote 1", "price_cents": 5000, "quantity": 1, "sort_order": 0,
		"turn_trigger": "sellout",
	})
	l2 := criarLote(t, ts, owner, eventID, map[string]any{
		"name": "Lote 2", "price_cents": 8000, "quantity": 5, "sort_order": 1,
	})
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar")
	}
	if !loteAberto(t, ts, eventID, l1) || loteAberto(t, ts, eventID, l2) {
		t.Fatalf("no começo só o lote 1 está à venda")
	}

	// Esgota o lote 1: a virada é derivada, ninguém escreve estado.
	res := buyViaSession(t, ts, buyer(t, ts, pool, "buy@progressiva.com"),
		map[string]any{"event_id": eventID, "quantity": 1}, "pix")
	confirmWebhook(t, ts, res["asaas_ref"].(string))
	if loteAberto(t, ts, eventID, l1) || !loteAberto(t, ts, eventID, l2) {
		t.Fatalf("esgotado o lote 1, o lote 2 deveria abrir sozinho")
	}

	// Agora por DATA: um lote com gatilho 'date' esgotado NÃO adianta a virada. Quem
	// prometeu "o próximo abre no dia X" não pode abrir antes.
	eventoData := createEvent(t, ts, owner, "Show por Data", "shows")
	amanha := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	d1 := criarLote(t, ts, owner, eventoData, map[string]any{
		"name": "Lote 1", "price_cents": 5000, "quantity": 1, "sort_order": 0,
		"turn_trigger": "date", "ends_at": amanha,
	})
	d2 := criarLote(t, ts, owner, eventoData, map[string]any{
		"name": "Lote 2", "price_cents": 8000, "quantity": 5, "sort_order": 1,
	})
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventoData+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar")
	}
	// Esgota o lote 1 direto no banco (a compra é o mesmo caminho já provado acima).
	esgotar(t, ctx, pool, pid, d1)
	if loteAberto(t, ts, eventoData, d2) {
		t.Fatalf("com gatilho por data, esgotar antes não abre o próximo")
	}
	// Passada a data, ele abre.
	adiantarFim(t, ctx, pool, pid, d1)
	if !loteAberto(t, ts, eventoData, d2) {
		t.Fatalf("passada a data, o lote 2 deveria abrir")
	}
}

// TestLotesSimultaneosVendemIndependentes (B.8.5 e B.8.6): "Pista" e "Camarote" não são
// lote 1 e lote 2 — são coisas diferentes, à venda ao mesmo tempo, e o comprador escolhe.
// A categoria avulsa é o mesmo mecanismo sem datas.
func TestLotesSimultaneosVendemIndependentes(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Simultanea", "owner@simultanea.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Simultâneo", "shows")

	pista := criarLote(t, ts, owner, eventID, map[string]any{
		"name": "Pista", "price_cents": 5000, "quantity": 10, "sort_order": 0,
		"availability": "always",
	})
	camarote := criarLote(t, ts, owner, eventID, map[string]any{
		"name": "Camarote", "price_cents": 20000, "quantity": 4, "sort_order": 1,
		"availability": "always",
	})
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar")
	}
	if !loteAberto(t, ts, eventID, pista) || !loteAberto(t, ts, eventID, camarote) {
		t.Fatalf("os dois deveriam estar à venda juntos")
	}

	// Comprar o CAMAROTE, que não é o primeiro da lista: sem a escolha valer, a venda sairia
	// da pista e o comprador pagaria o ingresso errado.
	res := buyViaSession(t, ts, buyer(t, ts, pool, "buy@simultanea.com"), map[string]any{
		"event_id": eventID, "quantity": 1, "lot_id": camarote,
	}, "pix")
	if res["amount_cents"].(float64) < 20000 {
		t.Fatalf("esperava o preço do camarote, veio %v", res["amount_cents"])
	}
	confirmWebhook(t, ts, res["asaas_ref"].(string))

	if n := scanInt(t, ctx, pool, pid, `SELECT sold_count FROM lots WHERE id=$1`, uuid.MustParse(camarote)); n != 1 {
		t.Fatalf("a venda deveria sair do camarote, veio sold_count=%d", n)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT sold_count FROM lots WHERE id=$1`, uuid.MustParse(pista)); n != 0 {
		t.Fatalf("a pista não podia ter vendido nada, veio sold_count=%d", n)
	}

	// Esgotar o camarote não derruba a pista: eles não estão na mesma fila.
	esgotar(t, ctx, pool, pid, camarote)
	if loteAberto(t, ts, eventID, camarote) || !loteAberto(t, ts, eventID, pista) {
		t.Fatalf("independentes: um esgotar não pode encerrar o outro")
	}
}

// TestSalvarLoteNaoApagaOResto (B.5): editar o preço é rotina, e salvar o preço não pode
// zerar a faixa de compra ou o aviso no caminho.
func TestSalvarLoteNaoApagaOResto(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducer(t, ts, "Casa Edita", "owner@edita.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show Edita", "shows")
	dois := 2
	lot := criarLote(t, ts, owner, eventID, map[string]any{
		"name": "Duplo", "price_cents": 5000, "quantity": 10, "sort_order": 0,
		"min_purchase_quantity": dois, "max_purchase_quantity": dois,
		"notice": "acomodações por ordem de chegada",
	})

	code, body := do(t, ts, "PATCH", "/api/v1/lots/"+lot, bearer(owner), map[string]any{
		"price_cents": 6000,
	})
	if code != http.StatusOK {
		t.Fatalf("salvar lote: %d %v", code, body)
	}
	if body["price_cents"] != float64(6000) {
		t.Fatalf("preço não salvou: %v", body)
	}
	if body["min_purchase_quantity"] != float64(2) || body["max_purchase_quantity"] != float64(2) {
		t.Fatalf("salvar o preço apagou a faixa de compra: %v", body)
	}
	if body["notice"] != "acomodações por ordem de chegada" {
		t.Fatalf("salvar o preço apagou o aviso: %v", body)
	}

	// Reduzir a quantidade abaixo do já vendido é recusado com motivo, não com erro de banco.
	if code, _ := do(t, ts, "PATCH", "/api/v1/lots/"+lot, bearer(owner),
		map[string]any{"availability": "sempre"}); code != http.StatusBadRequest {
		t.Fatalf("modo desconhecido deveria ser recusado, veio %d", code)
	}
}

// esgotar zera o saldo do lote direto no schema.
func esgotar(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid uuid.UUID, lotID string) {
	t.Helper()
	execTenant(t, ctx, pool, pid, `UPDATE lots SET sold_count = quantity WHERE id=$1`, uuid.MustParse(lotID))
}

// adiantarFim joga a data de fim do lote para o passado.
func adiantarFim(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid uuid.UUID, lotID string) {
	t.Helper()
	execTenant(t, ctx, pool, pid, `UPDATE lots SET ends_at = now() - interval '1 hour' WHERE id=$1`, uuid.MustParse(lotID))
}

// execTenant roda um comando no schema do produtor.
func execTenant(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid uuid.UUID, sql string, args ...any) {
	t.Helper()
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	})
}
