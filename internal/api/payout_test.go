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

// realizeEvent joga o evento para o passado. A realização não depende de ninguém clicar em
// nada: um evento que terminou ontem aconteceu, e o repasse dele não pode ficar preso por
// falta de clique.
func realizeEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid, eventID uuid.UUID) {
	t.Helper()
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `
			UPDATE events SET starts_at = now() - interval '2 days',
			                  ends_at   = now() - interval '2 days' + interval '3 hours'
			 WHERE id = $1`, eventID); err != nil {
			t.Fatalf("realizar evento: %v", err)
		}
	})
}

// payoutOf lê a obrigação de repasse do evento pelo painel do produtor, que é onde ela é
// recalculada na leitura.
func payoutOf(t *testing.T, ts *httptest.Server, owner string, eventID uuid.UUID) map[string]any {
	t.Helper()
	code, body := do(t, ts, "GET", "/api/v1/dash/events/"+eventID.String(), bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("painel do evento: %d %v", code, body)
	}
	p, _ := body["payout"].(map[string]any)
	if p == nil {
		t.Fatalf("painel sem repasse: %v", body)
	}
	return p
}

func cents(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

// TestVendaFicaComABilheteriaAteORepasse (A.9.1 e A.9.2): a compra não cria recebedor
// secundário — o valor integral fica com a plataforma — e a obrigação de repasse acumula
// enquanto o evento não acontece, virando pendente com vencimento na realização.
//
// É a decisão inteira num teste: enquanto houve split, o dinheiro de um evento que ainda
// não aconteceu já estava fora da conta de quem teria de devolvê-lo.
func TestVendaFicaComABilheteriaAteORepasse(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Retencao", "owner@retencao.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 2, "buy@retencao.com", "pix")

	p := payoutOf(t, ts, owner, eventID)
	if p["status"] != "accruing" {
		t.Fatalf("antes do evento o repasse acumula, veio %v", p["status"])
	}
	if p["due_at"] != nil {
		t.Fatalf("evento que não aconteceu não tem vencimento, veio %v", p["due_at"])
	}
	if cents(p["gross_face_cents"]) != 10000 || cents(p["net_due_cents"]) != 10000 {
		t.Fatalf("esperava face 10000 e líquido 10000, veio %v", p)
	}
	// A conveniência é da plataforma e NÃO entra no líquido do produtor.
	if cents(p["platform_fee_cents"]) != 1000 {
		t.Fatalf("esperava 1000 de conveniência para a plataforma, veio %v", p["platform_fee_cents"])
	}

	realizeEvent(t, ctx, pool, pid, eventID)
	p = payoutOf(t, ts, owner, eventID)
	if p["status"] != "pending" {
		t.Fatalf("evento realizado deveria deixar o repasse pendente, veio %v", p["status"])
	}
	due, err := time.Parse(time.RFC3339, p["due_at"].(string))
	if err != nil {
		t.Fatalf("vencimento ilegível: %v", p["due_at"])
	}
	// Default de 7 dias após a realização (que foi há 2 dias): vence daqui a ~5.
	if d := time.Until(due); d < 4*24*time.Hour || d > 6*24*time.Hour {
		t.Fatalf("vencimento fora do prazo de 7 dias após a realização: %v", due)
	}
	if cents(p["payout_delay_days"]) != 7 || p["payout_delay_inherited"] != true {
		t.Fatalf("esperava o padrão da casa de 7 dias, veio %v", p)
	}
}

// TestEstornoAbateDoRepasseSemCriarDivida (A.9.3): o estorno devolve dinheiro que está na
// conta da plataforma. Abate o face devolvido do repasse do evento e NÃO gera saldo devedor
// do produtor — o cenário "produtor já sacou" deixou de existir.
func TestEstornoAbateDoRepasseSemCriarDivida(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Abate", "owner@abate.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 2, "buy@abate.com", "pix")
	tickets := ticketsOf(t, ctx, pool, pid, eventID)

	orderID := orderOf(t, ctx, pool, pid, eventID)
	code, body := do(t, ts, "POST", "/api/v1/orders/"+orderID+"/refund", bearer(owner),
		map[string]any{"ticket_ids": []string{tickets[0]}, "reason": "desistiu"})
	if code != http.StatusOK {
		t.Fatalf("estorno: %d %v", code, body)
	}
	if body["recoverable_credit_cents"] != nil {
		t.Fatalf("estorno antes do repasse não cria crédito a recuperar, veio %v", body)
	}

	p := payoutOf(t, ts, owner, eventID)
	if cents(p["refunded_face_cents"]) != 5000 {
		t.Fatalf("esperava 5000 de face estornado, veio %v", p["refunded_face_cents"])
	}
	if cents(p["net_due_cents"]) != 5000 {
		t.Fatalf("esperava líquido 5000 (10000 − 5000), veio %v", p["net_due_cents"])
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM recoverable_credits`); n != 0 {
		t.Fatalf("estorno comum não pode virar dívida do produtor, veio %d crédito(s)", n)
	}
}

// TestEstornoParcialEmPedidoMisto (A.9.4): pedido com inteira e meia, estornando só a meia.
// O que sai do repasse é o valor DAQUELE ingresso, não a média do pedido.
func TestEstornoParcialEmPedidoMisto(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Mista", "owner@mista.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Misto", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 10000, 20, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar")
	}
	res := buyViaSession(t, ts, buyer(t, ts, pool, "buy@mista.com"), map[string]any{
		"event_id": eventID, "quantity": 2, "half_price_qty": 1,
	}, "pix")
	confirmWebhook(t, ts, res["asaas_ref"].(string))

	evID := uuid.MustParse(eventID)
	// Face do pedido: uma inteira (10000) + uma meia (5000).
	if v := payoutOf(t, ts, owner, evID); cents(v["gross_face_cents"]) != 15000 {
		t.Fatalf("esperava face 15000, veio %v", v["gross_face_cents"])
	}

	var meia string
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT id FROM tickets WHERE event_id=$1 AND half_price AND status='active'`, evID).Scan(&meia); err != nil {
			t.Fatalf("achar a meia: %v", err)
		}
	})
	orderID := orderOf(t, ctx, pool, pid, evID)
	if code, b := do(t, ts, "POST", "/api/v1/orders/"+orderID+"/refund", bearer(owner),
		map[string]any{"ticket_ids": []string{meia}, "reason": "só a meia"}); code != http.StatusOK {
		t.Fatalf("estorno: %d %v", code, b)
	}

	p := payoutOf(t, ts, owner, evID)
	if cents(p["refunded_face_cents"]) != 5000 {
		t.Fatalf("estornar a meia deveria abater 5000, veio %v", p["refunded_face_cents"])
	}
	if cents(p["net_due_cents"]) != 10000 {
		t.Fatalf("esperava líquido 10000 (a inteira que sobrou), veio %v", p["net_due_cents"])
	}
}

// TestCancelamentoLevaRepasseACancelled (A.9.5): cancelar o evento devolve o dinheiro de
// todo mundo e encerra o repasse. É onde o novo modelo mais ajuda — o dinheiro está com a
// plataforma, então a devolução em massa não depende de recuperar valor de ninguém.
func TestCancelamentoLevaRepasseACancelled(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Cancela", "owner@cancelarepasse.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 1, "buy@cancelarepasse.com", "pix")

	if code, b := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/cancel", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("cancelar: %d %v", code, b)
	}
	if s := scanText(t, ctx, pool, pid, `SELECT status FROM event_payouts WHERE event_id=$1`, eventID); s != "cancelled" {
		t.Fatalf("evento cancelado não tem repasse, veio %q", s)
	}
	// E o recálculo de rotina não ressuscita a linha: cancelamento é decisão de alguém.
	if p := payoutOf(t, ts, owner, eventID); p["status"] != "cancelled" {
		t.Fatalf("recálculo não pode desfazer o cancelamento, veio %v", p["status"])
	}
}

// TestEstornoAposRepassePagoGeraCredito (A.9.6): o caso de EXCEÇÃO. O repasse já foi
// liquidado e o comprador precisa ser devolvido assim mesmo. Vira crédito a recuperar,
// registrado e visível — e nada é abatido de repasse futuro automaticamente, porque não há
// repasse futuro garantido.
func TestEstornoAposRepassePagoGeraCredito(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Pago", "owner@repassepago.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 1, "buy@repassepago.com", "pix")
	realizeEvent(t, ctx, pool, pid, eventID)
	_ = payoutOf(t, ts, owner, eventID) // recalcula: accruing → pending

	admin := seedAdmin(t, ts, pool, "admin@repasse.com", "admin")
	if code, b := do(t, ts, "POST", "/api/v1/admin/producers/"+pid.String()+"/payouts/mark-paid", admin,
		map[string]any{"event_id": eventID.String(), "reference": "E2E-PIX-123"}); code != http.StatusOK {
		t.Fatalf("marcar pago: %d %v", code, b)
	}

	orderID := orderOf(t, ctx, pool, pid, eventID)
	code, body := do(t, ts, "POST", "/api/v1/admin/producers/"+pid.String()+"/orders/"+orderID+"/refund",
		admin, map[string]any{"reason": "contestação depois do repasse"})
	if code != http.StatusOK {
		t.Fatalf("estorno: %d %v", code, body)
	}
	if cents(body["recoverable_credit_cents"]) != 5000 {
		t.Fatalf("esperava crédito a recuperar de 5000, veio %v", body["recoverable_credit_cents"])
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM recoverable_credits WHERE settled_at IS NULL`); n != 1 {
		t.Fatalf("esperava 1 crédito a recuperar em aberto, veio %d", n)
	}
	// O repasse continua PAGO: o crédito não reabre nem desconta nada sozinho.
	if s := scanText(t, ctx, pool, pid, `SELECT status FROM event_payouts WHERE event_id=$1`, eventID); s != "paid" {
		t.Fatalf("o repasse pago não pode ser desfeito pelo estorno, veio %q", s)
	}
	// E o produtor vê a cobrança que vai chegar nele, em vez de descobrir depois.
	code, dash := do(t, ts, "GET", "/api/v1/dash/payouts", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("extrato: %d", code)
	}
	if list, _ := dash["recoverable_credits"].([]any); len(list) != 1 {
		t.Fatalf("esperava o crédito no extrato do produtor, veio %v", dash["recoverable_credits"])
	}
}

// TestRepasseRetidoExplicaOMotivo: reter dinheiro sem dizer por quê é indistinguível, para
// quem espera, de terem esquecido dele. O motivo vem de lista fechada e o produtor lê o
// texto correspondente.
func TestRepasseRetidoExplicaOMotivo(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Retida", "owner@retida.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 1, "buy@retida.com", "pix")
	realizeEvent(t, ctx, pool, pid, eventID)
	_ = payoutOf(t, ts, owner, eventID)

	admin := seedAdmin(t, ts, pool, "admin@repasse.com", "admin")
	if code, b := do(t, ts, "POST", "/api/v1/admin/producers/"+pid.String()+"/payouts/hold", admin,
		map[string]any{"event_id": eventID.String(), "reason": "disputa_aberta"}); code != http.StatusOK {
		t.Fatalf("reter: %d %v", code, b)
	}
	p := payoutOf(t, ts, owner, eventID)
	if p["status"] != "on_hold" {
		t.Fatalf("esperava repasse retido, veio %v", p["status"])
	}
	if msg, _ := p["hold_message"].(string); msg == "" {
		t.Fatalf("retenção sem explicação ao produtor: %v", p)
	}
	// Motivo fora da lista é recusado: retenção que ninguém consegue revisar depois não entra.
	if code, _ := do(t, ts, "POST", "/api/v1/admin/producers/"+pid.String()+"/payouts/hold", admin,
		map[string]any{"event_id": eventID.String(), "reason": "porque sim"}); code != http.StatusBadRequest {
		t.Fatalf("motivo livre deveria ser recusado, veio %d", code)
	}
	// Soltar devolve a linha ao cálculo normal.
	if code, b := do(t, ts, "POST", "/api/v1/admin/producers/"+pid.String()+"/payouts/release", admin,
		map[string]any{"event_id": eventID.String()}); code != http.StatusOK {
		t.Fatalf("soltar: %d %v", code, b)
	}
	if p := payoutOf(t, ts, owner, eventID); p["status"] != "pending" {
		t.Fatalf("solto, o repasse volta a pendente, veio %v", p["status"])
	}
}

// TestPrazoDeRepasseEhParametro: o prazo não é chumbado. Por produtor, com sobrescrita por
// evento — e mudar a política reescreve o vencimento já gravado, senão a tela do produtor
// mostraria uma promessa que ninguém mais tem.
func TestPrazoDeRepasseEhParametro(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Prazo", "owner@prazo.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 1, "buy@prazo.com", "pix")
	realizeEvent(t, ctx, pool, pid, eventID)
	_ = payoutOf(t, ts, owner, eventID)

	admin := seedAdmin(t, ts, pool, "admin@repasse.com", "admin")
	if code, b := do(t, ts, "PUT", "/api/v1/admin/producers/"+pid.String()+"/payout-delay", admin,
		map[string]any{"payout_delay_days": 30}); code != http.StatusOK {
		t.Fatalf("prazo da casa: %d %v", code, b)
	}
	p := payoutOf(t, ts, owner, eventID)
	if cents(p["payout_delay_days"]) != 30 || p["payout_delay_inherited"] != true {
		t.Fatalf("esperava 30 dias herdados do padrão da casa, veio %v", p)
	}
	due, _ := time.Parse(time.RFC3339, p["due_at"].(string))
	if d := time.Until(due); d < 27*24*time.Hour {
		t.Fatalf("o vencimento gravado deveria acompanhar o prazo novo: %v", due)
	}

	// Sobrescrita por evento vence o padrão.
	if code, b := do(t, ts, "PUT", "/api/v1/admin/producers/"+pid.String()+"/payout-delay", admin,
		map[string]any{"event_id": eventID.String(), "payout_delay_days": 2}); code != http.StatusOK {
		t.Fatalf("prazo do evento: %d %v", code, b)
	}
	p = payoutOf(t, ts, owner, eventID)
	if cents(p["payout_delay_days"]) != 2 || p["payout_delay_inherited"] != false {
		t.Fatalf("esperava 2 dias próprios do evento, veio %v", p)
	}
}
