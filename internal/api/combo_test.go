package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// comboEvent publica um evento de pista com um lote de faixa de quantidade fixa.
func comboEvent(t *testing.T, ts *httptest.Server, owner string, minQ int, maxQ *int, quantity int) string {
	t.Helper()
	eventID := createEvent(t, ts, owner, "Show Combo", "shows")
	body := map[string]any{
		"name": "Lote 1", "price_cents": 5000, "quantity": quantity, "sort_order": 0,
		"min_purchase_quantity": minQ,
	}
	if maxQ != nil {
		body["max_purchase_quantity"] = *maxQ
	}
	if code, b := do(t, ts, "POST", "/api/v1/events/"+eventID+"/lots", bearer(owner), body); code != http.StatusCreated {
		t.Fatalf("criar lote combo: %d %v", code, b)
	}
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar")
	}
	return eventID
}

// trySession tenta criar a sessão de checkout com uma quantidade e devolve o status. É o
// caminho real da compra, e é onde a faixa precisa ser respeitada.
func trySession(t *testing.T, ts *httptest.Server, eventID string, qty int) (int, map[string]any) {
	t.Helper()
	return do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil,
		map[string]any{"event_id": eventID, "quantity": qty})
}

// TestComboFaixaDeQuantidade: um lote com mínimo 2 e máximo 2 é o "ingresso duplo". Comprar
// 1 ou 3 é recusado; 2 passa. A recusa é do SERVIDOR — a UI travar o seletor não impede
// ninguém de chamar a API.
func TestComboFaixaDeQuantidade(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducer(t, ts, "Casa Combo", "owner@combo.com", "senha1234")
	dois := 2
	eventID := comboEvent(t, ts, owner, 2, &dois, 100)

	if code, body := trySession(t, ts, eventID, 1); code != http.StatusBadRequest {
		t.Fatalf("compra de 1 num duplo deveria ser recusada, veio %d %v", code, body)
	}
	if code, body := trySession(t, ts, eventID, 3); code != http.StatusBadRequest {
		t.Fatalf("compra de 3 num duplo deveria ser recusada, veio %d %v", code, body)
	}
	code, body := trySession(t, ts, eventID, 2)
	if code != http.StatusCreated {
		t.Fatalf("compra de 2 deveria passar: %d %v", code, body)
	}
}

// TestComboMensagemDizAFaixa: recusar sem dizer o que fazer obriga o comprador a adivinhar.
func TestComboMensagemDizAFaixa(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducer(t, ts, "Casa Msg", "owner@msgcombo.com", "senha1234")
	eventID := comboEvent(t, ts, owner, 3, nil, 100)

	_, body := trySession(t, ts, eventID, 1)
	msg, _ := body["error"].(string)
	if msg == "" || !containsAll(msg, "mínimo", "3") {
		t.Fatalf("a recusa precisa dizer a faixa, veio %q", msg)
	}
}

// TestComboGeraIngressosIndependentes: a compra de um duplo gera DOIS ingressos, cada um com
// seu QR. Combo é regra de compra, não vínculo entre ingressos.
func TestComboGeraIngressosIndependentes(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Dois QR", "owner@doisqr.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	dois := 2
	eventID := comboEvent(t, ts, owner, 2, &dois, 100)

	res := buyViaSession(t, ts, buyer(t, ts, pool, "buy@doisqr.com"), map[string]any{
		"event_id": eventID, "quantity": 2,
	}, "pix")
	confirmWebhook(t, ts, res["asaas_ref"].(string))

	evID := uuid.MustParse(eventID)
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE event_id=$1 AND status='active'`, evID); n != 2 {
		t.Fatalf("esperava 2 ingressos, veio %d", n)
	}
	// QRs distintos: o nonce é por ingresso, e dois iguais seriam o mesmo ingresso duas vezes.
	if n := scanInt(t, ctx, pool, pid, `SELECT count(DISTINCT qr_nonce) FROM tickets WHERE event_id=$1`, evID); n != 2 {
		t.Fatalf("esperava 2 QRs distintos, veio %d", n)
	}
	// O preço cadastrado é o UNITÁRIO: o comprador paga o dobro.
	if v := scanInt(t, ctx, pool, pid, `SELECT face_cents FROM orders WHERE event_id=$1`, evID); v != 10000 {
		t.Fatalf("esperava face de 10000 (5000 × 2), veio %d", v)
	}
}

// TestComboSaiDeVendaQuandoSaldoNaoAlcancaOMinimo: sobrar 1 lugar num lote de mínimo 2
// significa que aquele lote acabou. Continuar oferecendo levaria o comprador a uma recusa
// no fim do checkout.
func TestComboSaiDeVendaQuandoSaldoNaoAlcancaOMinimo(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Saldo", "owner@saldocombo.com", "senha1234")
	dois := 2
	eventID := comboEvent(t, ts, owner, 2, &dois, 3) // 3 lugares, duplo

	res := buyViaSession(t, ts, buyer(t, ts, pool, "buy@saldocombo.com"), map[string]any{
		"event_id": eventID, "quantity": 2,
	}, "pix")
	confirmWebhook(t, ts, res["asaas_ref"].(string))

	// Sobrou 1 lugar: o lote não alcança mais o mínimo e some da venda.
	code, body := do(t, ts, "GET", "/api/v1/public/events/"+eventID, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("evento público: %d", code)
	}
	if body["current_lot_id"] != nil {
		t.Fatalf("lote com saldo menor que o mínimo deveria sair de venda: %v", body["current_lot_id"])
	}
	if code, _ := trySession(t, ts, eventID, 2); code == http.StatusCreated {
		t.Fatal("não pode vender mais um duplo com 1 lugar restante")
	}
}

// TestComboComAssentoExigeNAssentos: em evento com mapa, o duplo exige dois assentos, e a
// regra anti-buraco continua valendo sobre o conjunto.
func TestComboComAssentoExigeNAssentos(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Assento Combo", "owner@assentocombo.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, seats, lotID := seatedEvent(t, ts, pool, owner, pid, 4)
	dois := 2
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `
			UPDATE lots SET min_purchase_quantity=2, max_purchase_quantity=$2 WHERE id=$1`, lotID, dois); err != nil {
			t.Fatalf("configurar combo: %v", err)
		}
		// Anti-buraco ligado: o conjunto escolhido não pode deixar assento órfão.
		if _, err := tx.Exec(ctx, `UPDATE events SET anti_hole=true WHERE id=$1`, eventID); err != nil {
			t.Fatalf("ligar anti-buraco: %v", err)
		}
	})
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar")
	}

	// Um assento só: a quantidade cai fora da faixa antes mesmo do mapa.
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil, map[string]any{
		"event_id": eventID.String(), "quantity": 1, "seat_ids": []string{seats[0].String()},
	}); code != http.StatusBadRequest {
		t.Fatalf("um assento num duplo deveria ser recusado")
	}
	// Dois assentos contíguos: passa.
	if code, b := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil, map[string]any{
		"event_id": eventID.String(), "quantity": 2,
		"seat_ids": []string{seats[0].String(), seats[1].String()},
	}); code != http.StatusCreated {
		t.Fatalf("dois assentos contíguos deveriam passar: %d %v", code, b)
	}
}

// TestComboMaximoMenorQueMinimoRecusado: faixa impossível é erro do produtor, e ele precisa
// ver isso ao cadastrar — não na primeira venda que não acontece.
func TestComboMaximoMenorQueMinimoRecusado(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducer(t, ts, "Casa Faixa Ruim", "owner@faixaruim.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show", "shows")
	if code, body := do(t, ts, "POST", "/api/v1/events/"+eventID+"/lots", bearer(owner), map[string]any{
		"name": "Lote", "price_cents": 5000, "quantity": 10,
		"min_purchase_quantity": 4, "max_purchase_quantity": 2,
	}); code != http.StatusBadRequest {
		t.Fatalf("máximo menor que o mínimo deveria ser recusado, veio %d %v", code, body)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
