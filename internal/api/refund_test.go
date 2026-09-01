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

	"github.com/jadersonmarc/sapienza-timbre/internal/chain"
	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
)

// execT roda um INSERT de apoio no schema do produtor.
func execT(t *testing.T, ctx context.Context, tx pgx.Tx, sql string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// orderOf devolve o id do pedido pago do evento.
func orderOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid, eventID uuid.UUID) string {
	t.Helper()
	var id string
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT id FROM orders WHERE event_id=$1 ORDER BY created_at DESC LIMIT 1`, eventID).Scan(&id); err != nil {
			t.Fatalf("pedido do evento: %v", err)
		}
	})
	return id
}

// seedCardSale monta, direto no schema, uma venda de cartão já paga. Existe para os testes
// que precisam de um pedido pago sem passar pelo caminho inteiro de compra.
func seedCardSale(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	var eventID, lotID, orderID, paymentID uuid.UUID
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		must(t, tx.QueryRow(ctx, `INSERT INTO events (title, category, category_id, starts_at, ends_at)
			VALUES ('Show Cartão','shows',(SELECT id FROM event_categories WHERE slug='shows'),
			        now() + interval '10 days', now() + interval '10 days' + interval '3 hours')
			RETURNING id`).Scan(&eventID))
		must(t, tx.QueryRow(ctx, `INSERT INTO lots (event_id, name, price_cents, quantity, sold_count)
			VALUES ($1,'Lote 1',5000,100,1) RETURNING id`, eventID).Scan(&lotID))
		must(t, tx.QueryRow(ctx, `INSERT INTO orders (event_id, buyer_email, total_cents, face_cents,
			platform_fee_cents, processing_fee_cents, status)
			VALUES ($1,'cartao@venda.com',5600,5000,500,100,'paid') RETURNING id`, eventID).Scan(&orderID))
		execT(t, ctx, tx, `INSERT INTO order_items (order_id, lot_id, quantity, unit_price_cents, half_price)
			VALUES ($1,$2,1,5000,false)`, orderID, lotID)
		must(t, tx.QueryRow(ctx, `INSERT INTO payments (order_id, method, amount_cents, status, asaas_ref)
			VALUES ($1,'credit_card',5600,'confirmed',$2) RETURNING id`, orderID, "fake_seed_"+orderID.String()).Scan(&paymentID))
		execT(t, ctx, tx, `INSERT INTO tickets (event_id, lot_id, order_id, transferable_after, status)
			VALUES ($1,$2,$3, now(), 'active')`, eventID, lotID, orderID)
		execT(t, ctx, tx, `INSERT INTO ledger_entries (event_id, order_id, payment_id, kind, amount_cents, available_at)
			VALUES ($1,$2,$3,'repasse',5000, NULL),
			       ($1,$2,$3,'taxa',600, now())`, eventID, orderID, paymentID)
	})
	return eventID, orderID.String()
}

// ticketsOf devolve os ingressos ativos de um evento, na ordem de emissão.
func ticketsOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid, eventID uuid.UUID) []string {
	t.Helper()
	var ids []string
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		rows, err := tx.Query(ctx, `SELECT id FROM tickets WHERE event_id=$1 AND status='active' ORDER BY created_at, id`, eventID)
		if err != nil {
			t.Fatalf("listar ingressos: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			ids = append(ids, id)
		}
	})
	return ids
}

// ledgerSum soma o razão por tipo de lançamento.
func ledgerSum(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid uuid.UUID, kind string) int64 {
	t.Helper()
	var v int64
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount_cents),0) FROM ledger_entries WHERE kind=$1`, kind).Scan(&v); err != nil {
			t.Fatalf("razão %s: %v", kind, err)
		}
	})
	return v
}

// soldEvent monta um evento com assentos, publica, vende e confirma. Devolve o evento, os
// assentos e a referência da cobrança.
func soldEvent(t *testing.T, ts *httptest.Server, pool *pgxpool.Pool, owner string, pid uuid.UUID,
	seatCount int, buyerEmail, method string) (uuid.UUID, []uuid.UUID, string) {
	t.Helper()
	eventID, seats, _ := seatedEvent(t, ts, pool, owner, pid, seatCount)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	ids := make([]string, 0, seatCount)
	for _, s := range seats {
		ids = append(ids, s.String())
	}
	body := buyViaSession(t, ts, buyer(t, ts, pool, buyerEmail), map[string]any{
		"event_id": eventID.String(), "quantity": seatCount, "seat_ids": ids,
	}, method)
	ref, _ := body["asaas_ref"].(string)
	confirmWebhook(t, ts, ref)
	return eventID, seats, ref
}

// TestRefundTotalLedgerSigns: o estorno total devolve capacidade, queima o ingresso e
// lança o razão com o sinal certo em CADA ponta — face pelo produtor, conveniência pela
// plataforma. Antes havia um lançamento só, com o total do comprador, e ele descontava do
// produtor os 10% que nunca foram dele.
func TestRefundTotalLedgerSigns(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Sinal", "owner@sinal.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, seats, _ := soldEvent(t, ts, pool, owner, pid, 1, "buy@sinal.com", "pix")

	faceBefore := ledgerSum(t, ctx, pool, pid, "repasse")
	if faceBefore != 5000 {
		t.Fatalf("repasse esperado 5000 (face), veio %d", faceBefore)
	}

	orderID := orderOf(t, ctx, pool, pid, eventID)
	// Conveniência é tudo o que o comprador pagou acima do face: taxa da plataforma mais a
	// tarifa estimada do gateway. Volta integral — ninguém lucra com cancelamento.
	convenience := int64(scanInt(t, ctx, pool, pid, `SELECT total_cents - face_cents FROM orders WHERE id=$1`, uuid.MustParse(orderID)))
	code, body := do(t, ts, "POST", "/api/v1/orders/"+orderID+"/refund", bearer(owner), map[string]any{
		"reason": "desistência do comprador",
	})
	if code != http.StatusOK {
		t.Fatalf("estorno: %d %v", code, body)
	}
	if body["scope"] != "total" {
		t.Fatalf("esperava escopo total, veio %v", body["scope"])
	}

	// O produtor devolve o FACE, não o total do comprador.
	if got := ledgerSum(t, ctx, pool, pid, "estorno"); got != -faceBefore {
		t.Fatalf("estorno esperado %d (face), veio %d", -faceBefore, got)
	}
	// A conveniência volta pela plataforma, em linha própria.
	if got := ledgerSum(t, ctx, pool, pid, "estorno_taxa"); got != -convenience {
		t.Fatalf("estorno_taxa esperado %d (conveniência), veio %d", -convenience, got)
	}

	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='burned'`); n != 1 {
		t.Fatalf("esperava 1 ingresso queimado, veio %d", n)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM orders WHERE status='refunded'`); n != 1 {
		t.Fatalf("esperava ordem estornada, veio %d", n)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT sold_count FROM lots WHERE event_id=$1`, eventID); n != 0 {
		t.Fatalf("esperava capacidade devolvida ao lote, sold_count=%d", n)
	}
	// Assento liberado: dá para segurar de novo.
	if _, err := holdTx(ctx, pool, pid, eventID, seats[:1], time.Minute); err != nil {
		t.Fatalf("re-hold após estorno: %v", err)
	}
}

// TestRefundPartialKeepsRest: estornar 1 de 4 não pode derrubar os outros 3 — eles
// continuam válidos e entram na portaria.
func TestRefundPartialKeepsRest(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Parcial", "owner@parcial.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 4, "buy@parcial.com", "pix")

	tickets := ticketsOf(t, ctx, pool, pid, eventID)
	if len(tickets) != 4 {
		t.Fatalf("esperava 4 ingressos, veio %d", len(tickets))
	}
	orderID := orderOf(t, ctx, pool, pid, eventID)

	code, body := do(t, ts, "POST", "/api/v1/orders/"+orderID+"/refund", bearer(owner), map[string]any{
		"ticket_ids": []string{tickets[0]}, "reason": "um ingresso a menos",
	})
	if code != http.StatusOK {
		t.Fatalf("estorno parcial: %d %v", code, body)
	}
	if body["scope"] != "partial" {
		t.Fatalf("esperava escopo partial, veio %v", body["scope"])
	}

	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='burned'`); n != 1 {
		t.Fatalf("esperava 1 queimado, veio %d", n)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='active'`); n != 3 {
		t.Fatalf("esperava 3 ativos, veio %d", n)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM orders WHERE status='partially_refunded'`); n != 1 {
		t.Fatalf("esperava ordem parcialmente estornada, veio %d", n)
	}
	// Só um assento liberado.
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM seat_occupancy WHERE NOT released`); n != 3 {
		t.Fatalf("esperava 3 ocupações vivas, veio %d", n)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT sold_count FROM lots WHERE event_id=$1`, eventID); n != 3 {
		t.Fatalf("esperava sold_count=3, veio %d", n)
	}
	// O pagamento NÃO vira 'refunded' enquanto sobrar ingresso.
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM payments WHERE status='confirmed'`); n != 1 {
		t.Fatalf("pagamento não deveria virar estornado no parcial")
	}
}

// TestRefundPartialsCloseWithoutLostCent: quatro parciais fecham a ordem e a soma bate
// EXATAMENTE o face. Dividir o total a cada estorno deixaria centavo sobrando ou faltando.
func TestRefundPartialsCloseWithoutLostCent(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Centavo", "owner@centavo.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 3, "buy@centavo.com", "pix")

	face := ledgerSum(t, ctx, pool, pid, "repasse")
	tickets := ticketsOf(t, ctx, pool, pid, eventID)
	orderID := orderOf(t, ctx, pool, pid, eventID)
	convenience := int64(scanInt(t, ctx, pool, pid, `SELECT total_cents - face_cents FROM orders WHERE id=$1`, uuid.MustParse(orderID)))

	for i, tk := range tickets {
		code, body := do(t, ts, "POST", "/api/v1/orders/"+orderID+"/refund", bearer(owner), map[string]any{
			"ticket_ids": []string{tk}, "reason": "parcial",
		})
		if code != http.StatusOK {
			t.Fatalf("parcial %d: %d %v", i, code, body)
		}
	}

	if got := ledgerSum(t, ctx, pool, pid, "estorno"); got != -face {
		t.Fatalf("soma dos parciais %d != face %d — sobrou ou faltou centavo", got, -face)
	}
	if got := ledgerSum(t, ctx, pool, pid, "estorno_taxa"); got != -convenience {
		t.Fatalf("conveniência devolvida %d != cobrada %d", got, -convenience)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM orders WHERE status='refunded'`); n != 1 {
		t.Fatalf("último parcial deveria fechar a ordem em refunded")
	}
}

// TestRefundIdempotentOnDoubleFire: duplo clique vira um estorno só no gateway. A garantia
// é do índice único em refund_tickets, não de checagem da aplicação.
func TestRefundIdempotentOnDoubleFire(t *testing.T) {
	ts, pool, gw := setupWithGateway(t)
	_, owner := createProducer(t, ts, "Casa Duplo", "owner@duplo.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, ref := soldEvent(t, ts, pool, owner, pid, 1, "buy@duplo.com", "pix")
	orderID := orderOf(t, ctx, pool, pid, eventID)

	if code, body := do(t, ts, "POST", "/api/v1/orders/"+orderID+"/refund", bearer(owner),
		map[string]any{"reason": "primeiro"}); code != http.StatusOK {
		t.Fatalf("primeiro estorno: %d %v", code, body)
	}
	// O segundo não acha mais ingresso ativo: 404, sem tocar no gateway.
	if code, _ := do(t, ts, "POST", "/api/v1/orders/"+orderID+"/refund", bearer(owner),
		map[string]any{"reason": "segundo"}); code != http.StatusNotFound {
		t.Fatalf("segundo estorno: esperava 404, veio %d", code)
	}
	if n := gw.RefundCalls(ref); n != 1 {
		t.Fatalf("esperava 1 chamada de estorno no gateway, veio %d", n)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='burned'`); n != 1 {
		t.Fatalf("esperava 1 queimado, veio %d", n)
	}
}

// TestRefundWebhookEchoDoesNotReapply: o aviso do gateway sobre o estorno que NÓS
// originamos não pode ser lido como um estorno externo — senão queimaria o que sobrou.
func TestRefundWebhookEchoDoesNotReapply(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Eco", "owner@eco.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, ref := soldEvent(t, ts, pool, owner, pid, 2, "buy@eco.com", "pix")

	tickets := ticketsOf(t, ctx, pool, pid, eventID)
	orderID := orderOf(t, ctx, pool, pid, eventID)
	if code, body := do(t, ts, "POST", "/api/v1/orders/"+orderID+"/refund", bearer(owner),
		map[string]any{"ticket_ids": []string{tickets[0]}, "reason": "parcial"}); code != http.StatusOK {
		t.Fatalf("estorno parcial: %d %v", code, body)
	}
	estornoAntes := ledgerSum(t, ctx, pool, pid, "estorno")

	// Eco do gateway.
	if code, _ := do(t, ts, "POST", "/api/v1/webhooks/asaas", nil, map[string]any{
		"id": "evt_" + uuid.NewString(), "asaas_ref": ref, "refunded": true, "type": "PAYMENT_REFUNDED",
	}); code != http.StatusOK {
		t.Fatalf("webhook eco: %d", code)
	}

	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='active'`); n != 1 {
		t.Fatalf("o eco não pode queimar o ingresso que sobrou; ativos=%d", n)
	}
	if got := ledgerSum(t, ctx, pool, pid, "estorno"); got != estornoAntes {
		t.Fatalf("o eco duplicou o lançamento: %d → %d", estornoAntes, got)
	}
}

// TestRefundCheckedInGuard: ingresso que já entrou não é estornável pelo produtor; o admin
// passa, com motivo.
func TestRefundCheckedInGuard(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Portaria", "owner@portaria.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 1, "buy@portaria.com", "pix")

	tickets := ticketsOf(t, ctx, pool, pid, eventID)
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `INSERT INTO checkins (ticket_id, is_reentry) VALUES ($1,false)`, tickets[0]); err != nil {
			t.Fatalf("registrar entrada: %v", err)
		}
	})

	orderID := orderOf(t, ctx, pool, pid, eventID)
	if code, _ := do(t, ts, "POST", "/api/v1/orders/"+orderID+"/refund", bearer(owner),
		map[string]any{"reason": "tentativa"}); code != http.StatusConflict {
		t.Fatalf("produtor estornando entrada registrada: esperava 409, veio %d", code)
	}

	admin := seedAdmin(t, ts, pool, "admin@portaria.com", "admin")
	// Sem motivo, o admin também não passa.
	if code, _ := do(t, ts, "POST", "/api/v1/admin/producers/"+pid.String()+"/orders/"+orderID+"/refund", admin, nil); code != http.StatusBadRequest {
		t.Fatalf("admin sem motivo: esperava 400, veio %d", code)
	}
	if code, body := do(t, ts, "POST", "/api/v1/admin/producers/"+pid.String()+"/orders/"+orderID+"/refund", admin,
		map[string]any{"reason": "cliente passou mal na entrada"}); code != http.StatusOK {
		t.Fatalf("admin com motivo: %d %v", code, body)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='burned'`); n != 1 {
		t.Fatalf("esperava ingresso queimado pelo admin, veio %d", n)
	}
}

// TestRefundAfterCloseRepublishesAttestation: estorno depois do fechamento gera atestado
// versão 2, e a versão 1 continua acessível. Sem isso, a comprovação de público mente.
func TestRefundAfterCloseRepublishesAttestation(t *testing.T) {
	// setupAttest registra a chave de atestação: a verificação pública resolve a chave pelo
	// key_id do atestado, e sem ela até a v1 responderia erro.
	ts, pool, _, _ := setupAttest(t, chain.NoopAnchorer{}, chain.AnchorModeOff)
	_, owner := createProducer(t, ts, "Casa Atestado", "owner@atestado.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 2, "buy@atestado.com", "pix")

	// Fecha o evento: o atestado v1 registra 2 vendidos.
	code, first := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/close", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("fechar evento: %d %v", code, first)
	}
	firstID, _ := first["id"].(string)

	tickets := ticketsOf(t, ctx, pool, pid, eventID)
	orderID := orderOf(t, ctx, pool, pid, eventID)
	if code, body := do(t, ts, "POST", "/api/v1/orders/"+orderID+"/refund", bearer(owner),
		map[string]any{"ticket_ids": []string{tickets[0]}, "reason": "após o fechamento"}); code != http.StatusOK {
		t.Fatalf("estorno após fechamento: %d %v", code, body)
	}

	var version int
	var supersedes *string
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `
			SELECT version, supersedes_id::text FROM event_attestations
			 WHERE event_id=$1 AND supersedes_id IS NOT NULL`, eventID).Scan(&version, &supersedes); err != nil {
			t.Fatalf("atestado novo: %v", err)
		}
	})
	if version != 2 {
		t.Fatalf("esperava atestado versão 2, veio %d", version)
	}
	if supersedes == nil || *supersedes != firstID {
		t.Fatalf("versão 2 deveria apontar para a v1 (%s), veio %v", firstID, supersedes)
	}
	// A v1 continua acessível: correção é versão nova, nunca edição.
	if code, b := do(t, ts, "GET", "/api/v1/public/attestations/"+firstID, nil, nil); code != http.StatusOK {
		t.Fatalf("v1 deveria continuar acessível, veio %d %v", code, b)
	}
}

// TestRefundPartialRestStillAdmits: o que sobra de um estorno parcial precisa ENTRAR na
// portaria. Conferir só o status no banco não prova nada para quem está na fila do evento
// — o ingresso estornado tem de ser recusado e os outros admitidos.
func TestRefundPartialRestStillAdmits(t *testing.T) {
	ts, pool, signer := setupSigned(t)
	_, owner := createProducer(t, ts, "Casa Fila", "owner@fila.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 3, "buy@fila.com", "pix")
	_ = signer

	tickets := ticketsOf(t, ctx, pool, pid, eventID)
	tokens := make(map[string]string, len(tickets))
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		for _, id := range tickets {
			tok, err := ticketing.TicketToken(ctx, tx, uuid.MustParse(id))
			if err != nil {
				t.Fatalf("token do ingresso: %v", err)
			}
			tokens[id] = tok
		}
	})

	orderID := orderOf(t, ctx, pool, pid, eventID)
	if code, body := do(t, ts, "POST", "/api/v1/orders/"+orderID+"/refund", bearer(owner),
		map[string]any{"ticket_ids": []string{tickets[0]}, "reason": "um a menos"}); code != http.StatusOK {
		t.Fatalf("estorno parcial: %d %v", code, body)
	}

	// O estornado não entra.
	_, body := do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner),
		map[string]any{"token": tokens[tickets[0]], "gate": "G1"})
	if v := verdict(body); v == "admitted" {
		t.Fatalf("ingresso estornado não pode entrar, veredito %s", v)
	}
	// Os outros dois entram normalmente.
	for _, id := range tickets[1:] {
		_, body := do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner),
			map[string]any{"token": tokens[id], "gate": "G1"})
		if v := verdict(body); v != "admitted" {
			t.Fatalf("ingresso não estornado deveria entrar, veredito %s (%v)", v, body)
		}
	}
}

// TestEcoReconhecidoPelaChave: quando o aviso do gateway traz as chaves das devoluções, o
// reconhecimento é EXATO — não depende de janela de tempo, que é palpite.
//
// Medido no sandbox: o Asaas não emite id de estorno, e a identidade é a description que
// enviamos. Se ela viajar no aviso, é por ela que se sabe o que é eco do nosso.
func TestEcoReconhecidoPelaChave(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Chave", "owner@chave.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, ref := soldEvent(t, ts, pool, owner, pid, 2, "buy@chave.com", "pix")

	tickets := ticketsOf(t, ctx, pool, pid, eventID)
	orderID := orderOf(t, ctx, pool, pid, eventID)
	code, body := do(t, ts, "POST", "/api/v1/orders/"+orderID+"/refund", bearer(owner),
		map[string]any{"ticket_ids": []string{tickets[0]}, "reason": "parcial"})
	if code != http.StatusOK {
		t.Fatalf("estorno parcial: %d %v", code, body)
	}
	refundID, _ := body["id"].(string)

	// O aviso chega com a chave da NOSSA devolução: é eco, e não pode tocar no que sobrou.
	if code, _ := do(t, ts, "POST", "/api/v1/webhooks/asaas", nil, map[string]any{
		"id": "evt_" + uuid.NewString(), "asaas_ref": ref, "refunded": true,
		"type": "PAYMENT_REFUNDED", "refund_keys": []string{"timbre:refund:" + refundID},
	}); code != http.StatusOK {
		t.Fatalf("webhook eco")
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='active'`); n != 1 {
		t.Fatalf("o eco não pode queimar o ingresso restante; ativos=%d", n)
	}

	// Agora um aviso com chave DESCONHECIDA: devolução feita por fora, e ela vale.
	if code, _ := do(t, ts, "POST", "/api/v1/webhooks/asaas", nil, map[string]any{
		"id": "evt_" + uuid.NewString(), "asaas_ref": ref, "refunded": true,
		"type": "PAYMENT_REFUNDED", "refund_keys": []string{"outro-sistema:qualquer"},
	}); code != http.StatusOK {
		t.Fatalf("webhook externo")
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='active'`); n != 0 {
		t.Fatalf("devolução feita por fora precisa valer; ativos=%d", n)
	}
}

// TestWebhookDeCobrancaAlheiaEhIgnorado: o webhook do gateway é por CONTA, não por
// aplicação. Enquanto Timbre e console dividirem a mesma conta, cada um recebe os eventos do
// outro — inclusive estorno. Cobrança que não está no nosso índice precisa ser reconhecida
// com 200 e não mexer em nada: responder erro faria o gateway reenviar para sempre.
func TestWebhookDeCobrancaAlheiaEhIgnorado(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Alheia", "owner@alheia.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	soldEvent(t, ts, pool, owner, pid, 1, "buy@alheia.com", "pix")

	antes := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='active'`)

	for _, tipo := range []map[string]any{
		{"confirmed": true, "type": "PAYMENT_CONFIRMED"},
		{"refunded": true, "type": "PAYMENT_REFUNDED"},
	} {
		body := map[string]any{"id": "evt_" + uuid.NewString(), "asaas_ref": "pay_de_outra_aplicacao"}
		for k, v := range tipo {
			body[k] = v
		}
		if code, b := do(t, ts, "POST", "/api/v1/webhooks/asaas", nil, body); code != http.StatusOK {
			t.Fatalf("evento alheio precisa ser reconhecido com 200 (senão o gateway reenvia para sempre): %d %v", code, b)
		}
	}

	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='active'`); n != antes {
		t.Fatalf("evento de cobrança alheia não pode tocar nos nossos ingressos: %d → %d", antes, n)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM refunds`); n != 0 {
		t.Fatalf("evento alheio não pode criar estorno, veio %d", n)
	}
}
