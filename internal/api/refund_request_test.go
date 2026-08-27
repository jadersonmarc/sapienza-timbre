package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// backdateOrder envelhece a compra para cair fora da janela de arrependimento. Mexer no
// relógio do teste seria pior: a janela é contada em dias, e um teste que dorme não presta.
func backdateOrder(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid uuid.UUID, orderID string, days int) {
	t.Helper()
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `
			UPDATE orders SET created_at = created_at - make_interval(days => $2) WHERE id=$1`,
			uuid.MustParse(orderID), days); err != nil {
			t.Fatalf("envelhecer a compra: %v", err)
		}
	})
}

// TestPolicyRespeitaPisoLegal: a janela de arrependimento pode ser ESTICADA, nunca
// encurtada. Sete dias é o art. 49 do CDC, e deixar isso na mão do produtor seria vender
// uma promessa que a lei não permite.
func TestPolicyRespeitaPisoLegal(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducer(t, ts, "Casa Politica", "owner@politica.com", "senha1234")

	if code, body := do(t, ts, "PUT", "/api/v1/refund-policy", bearer(owner),
		map[string]any{"withdrawal_window_days": 3}); code != http.StatusBadRequest {
		t.Fatalf("janela abaixo do piso legal deveria ser recusada, veio %d %v", code, body)
	}
	if code, body := do(t, ts, "PUT", "/api/v1/refund-policy", bearer(owner),
		map[string]any{"withdrawal_window_days": 30, "discretionary_response_hours": 48,
			"refund_gateway_fee_bearer": "producer", "producer_discretionary_enabled": true}); code != http.StatusOK {
		t.Fatalf("janela maior que o piso deveria ser aceita: %d %v", code, body)
	}

	code, body := do(t, ts, "GET", "/api/v1/refund-policy", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("ler política: %d", code)
	}
	p, _ := body["policy"].(map[string]any)
	if p["withdrawal_window_days"] != float64(30) {
		t.Fatalf("esperava janela 30, veio %v", p["withdrawal_window_days"])
	}
	if body["configured"] != true {
		t.Fatalf("política gravada deveria vir como configurada")
	}
}

// TestPolicyEventoHerdaDefault: evento sem política própria segue a do produtor, e a do
// evento tem precedência quando existe.
func TestPolicyEventoHerdaDefault(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Heranca", "owner@heranca.com", "senha1234")
	pid := producerID(t, ts, owner)
	eventID, _, _ := seatedEvent(t, ts, pool, owner, pid, 1)
	// A leitura pública resolve o produtor pelo diretório, que só tem evento publicado.
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}

	if code, _ := do(t, ts, "PUT", "/api/v1/refund-policy", bearer(owner),
		map[string]any{"withdrawal_window_days": 15}); code != http.StatusOK {
		t.Fatalf("gravar default do produtor")
	}
	// Sem política própria, o evento entrega a do produtor na leitura pública.
	code, pub := do(t, ts, "GET", "/api/v1/public/events/"+eventID.String()+"/refund-policy", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("política pública: %d %v", code, pub)
	}
	if pub["withdrawal_window_days"] != float64(15) {
		t.Fatalf("evento deveria herdar 15 dias, veio %v", pub["withdrawal_window_days"])
	}

	// Com política própria, ela vence.
	if code, _ := do(t, ts, "PUT", "/api/v1/events/"+eventID.String()+"/refund-policy", bearer(owner),
		map[string]any{"withdrawal_window_days": 20}); code != http.StatusOK {
		t.Fatalf("gravar política do evento")
	}
	_, pub = do(t, ts, "GET", "/api/v1/public/events/"+eventID.String()+"/refund-policy", nil, nil)
	if pub["withdrawal_window_days"] != float64(20) {
		t.Fatalf("política do evento deveria vencer, veio %v", pub["withdrawal_window_days"])
	}
	// O que é acerto entre casa e plataforma não vaza para o comprador.
	if _, ok := pub["refund_gateway_fee_bearer"]; ok {
		t.Fatalf("quem absorve a tarifa não é assunto do comprador: %v", pub)
	}
}

// TestPedidoDentroDaJanelaEhDireito: arrependimento dentro da janela não passa pelo
// produtor. Se dependesse de aprovação, deixaria de ser direito.
func TestPedidoDentroDaJanelaEhDireito(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Direito", "owner@direito.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 1, "buy@direito.com", "pix")
	orderID := orderOf(t, ctx, pool, pid, eventID)
	buyerHdr := buyer(t, ts, pool, "buy@direito.com")

	code, body := do(t, ts, "POST", "/api/v1/public/me/orders/"+orderID+"/refund-request", buyerHdr,
		map[string]any{"reason": "desisti"})
	if code != http.StatusOK {
		t.Fatalf("arrependimento na janela deveria executar na hora: %d %v", code, body)
	}
	req, _ := body["request"].(map[string]any)
	if req["track"] != "withdrawal" {
		t.Fatalf("esperava trilha withdrawal, veio %v", req["track"])
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='burned'`); n != 1 {
		t.Fatalf("o ingresso deveria estar queimado, veio %d", n)
	}
	if s := scanStr(t, ctx, pool, pid, `SELECT status FROM refund_requests LIMIT 1`); s != "completed" {
		t.Fatalf("pedido deveria estar completed, veio %s", s)
	}
}

// TestPedidoForaDaJanelaVaiParaFila: fora da janela é liberalidade — fica pendente, e o
// SILÊNCIO do produtor não aprova. Aprovação automática moveria dinheiro sem decisão.
func TestPedidoForaDaJanelaVaiParaFila(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Fila Estorno", "owner@filaestorno.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 1, "buy@filaestorno.com", "pix")
	orderID := orderOf(t, ctx, pool, pid, eventID)
	backdateOrder(t, ctx, pool, pid, orderID, 30)
	buyerHdr := buyer(t, ts, pool, "buy@filaestorno.com")

	code, body := do(t, ts, "POST", "/api/v1/public/me/orders/"+orderID+"/refund-request", buyerHdr,
		map[string]any{"reason": "não vou mais"})
	if code != http.StatusAccepted {
		t.Fatalf("fora da janela deveria ficar pendente: %d %v", code, body)
	}
	req, _ := body["request"].(map[string]any)
	if req["track"] != "discretionary" || req["status"] != "pending" {
		t.Fatalf("esperava discretionary/pending, veio %v", req)
	}
	// Nada aconteceu com o ingresso enquanto ninguém decidiu.
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='active'`); n != 1 {
		t.Fatalf("o ingresso não pode ser queimado antes da decisão")
	}
	if req["responds_by"] == nil {
		t.Fatalf("liberalidade precisa de prazo de resposta: %v", req)
	}

	// A fila do produtor mostra o pedido.
	code, list := do(t, ts, "GET", "/api/v1/refund-requests?status=pending", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("fila: %d", code)
	}
	if items, _ := list["requests"].([]any); len(items) != 1 {
		t.Fatalf("esperava 1 pedido na fila, veio %v", list["requests"])
	}

	// Aprovar executa o estorno.
	reqID, _ := req["id"].(string)
	if code, body := do(t, ts, "POST", "/api/v1/refund-requests/"+reqID+"/approve", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("aprovar: %d %v", code, body)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='burned'`); n != 1 {
		t.Fatalf("aprovado deveria queimar o ingresso, veio %d", n)
	}
}

// TestRecusaExigeMotivo: recusar sem dizer por quê é o que faz o comprador voltar pelo
// canal mais caro. E recusado não pode mexer no ingresso.
func TestRecusaExigeMotivo(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Recusa", "owner@recusa.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 1, "buy@recusa.com", "pix")
	orderID := orderOf(t, ctx, pool, pid, eventID)
	backdateOrder(t, ctx, pool, pid, orderID, 30)
	buyerHdr := buyer(t, ts, pool, "buy@recusa.com")

	_, body := do(t, ts, "POST", "/api/v1/public/me/orders/"+orderID+"/refund-request", buyerHdr,
		map[string]any{"reason": "mudei de ideia"})
	req, _ := body["request"].(map[string]any)
	reqID, _ := req["id"].(string)

	if code, _ := do(t, ts, "POST", "/api/v1/refund-requests/"+reqID+"/reject", bearer(owner), nil); code != http.StatusBadRequest {
		t.Fatalf("recusa sem motivo deveria ser 400, veio %d", code)
	}
	if code, b := do(t, ts, "POST", "/api/v1/refund-requests/"+reqID+"/reject", bearer(owner),
		map[string]any{"reason": "evento em dois dias, casa já contratada"}); code != http.StatusOK {
		t.Fatalf("recusa com motivo: %d %v", code, b)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='active'`); n != 1 {
		t.Fatalf("recusado não pode queimar o ingresso")
	}
	// Decidido não se decide de novo.
	if code, _ := do(t, ts, "POST", "/api/v1/refund-requests/"+reqID+"/approve", bearer(owner), nil); code != http.StatusConflict {
		t.Fatalf("pedido já decidido deveria dar 409, veio %d", code)
	}
}

// TestLiberalidadeDesligadaRecusaNaHora: com a porta fechada, o pedido fora da janela é
// recusado na hora e com o motivo — melhor que ficar parado numa fila que ninguém olha.
func TestLiberalidadeDesligadaRecusaNaHora(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Fechada", "owner@fechada.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	if code, _ := do(t, ts, "PUT", "/api/v1/refund-policy", bearer(owner),
		map[string]any{"withdrawal_window_days": 7, "producer_discretionary_enabled": false,
			"discretionary_response_hours": 72}); code != http.StatusOK {
		t.Fatalf("gravar política")
	}
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 1, "buy@fechada.com", "pix")
	orderID := orderOf(t, ctx, pool, pid, eventID)
	backdateOrder(t, ctx, pool, pid, orderID, 30)
	buyerHdr := buyer(t, ts, pool, "buy@fechada.com")

	if code, body := do(t, ts, "POST", "/api/v1/public/me/orders/"+orderID+"/refund-request", buyerHdr,
		map[string]any{"reason": "tentativa"}); code != http.StatusConflict {
		t.Fatalf("esperava 409 com a liberalidade desligada, veio %d %v", code, body)
	}
}

// TestPedidoDeOutroCompradorNaoAbre: IDOR. A compra é de quem comprou.
func TestPedidoDeOutroCompradorNaoAbre(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa IDOR", "owner@idor.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 1, "dono@idor.com", "pix")
	orderID := orderOf(t, ctx, pool, pid, eventID)

	intruso := buyer(t, ts, pool, "intruso@idor.com")
	if code, _ := do(t, ts, "POST", "/api/v1/public/me/orders/"+orderID+"/refund-request", intruso,
		map[string]any{"reason": "não é minha"}); code != http.StatusNotFound {
		t.Fatalf("compra de outra pessoa deveria dar 404, veio %d", code)
	}
}

// TestPedidoVivoUnicoPorCompra: duplo clique não vira dois pedidos. A garantia é do índice
// único parcial, não de checagem da aplicação.
func TestPedidoVivoUnicoPorCompra(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Duplo Pedido", "owner@duplopedido.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 1, "buy@duplopedido.com", "pix")
	orderID := orderOf(t, ctx, pool, pid, eventID)
	backdateOrder(t, ctx, pool, pid, orderID, 30)
	buyerHdr := buyer(t, ts, pool, "buy@duplopedido.com")

	if code, _ := do(t, ts, "POST", "/api/v1/public/me/orders/"+orderID+"/refund-request", buyerHdr,
		map[string]any{"reason": "primeiro"}); code != http.StatusAccepted {
		t.Fatalf("primeiro pedido")
	}
	if code, _ := do(t, ts, "POST", "/api/v1/public/me/orders/"+orderID+"/refund-request", buyerHdr,
		map[string]any{"reason": "segundo"}); code != http.StatusConflict {
		t.Fatalf("segundo pedido deveria dar 409, veio %d", code)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM refund_requests`); n != 1 {
		t.Fatalf("esperava 1 pedido, veio %d", n)
	}
}

// TestAuditoriaGuardaCadaTransicao: é o que o produtor mostra quando o comprador reclama, e
// o que a plataforma mostra quando o produtor reclama. Append-only.
func TestAuditoriaGuardaCadaTransicao(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Auditoria", "owner@auditoria.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 1, "buy@auditoria.com", "pix")
	orderID := orderOf(t, ctx, pool, pid, eventID)
	backdateOrder(t, ctx, pool, pid, orderID, 30)
	buyerHdr := buyer(t, ts, pool, "buy@auditoria.com")

	_, body := do(t, ts, "POST", "/api/v1/public/me/orders/"+orderID+"/refund-request", buyerHdr,
		map[string]any{"reason": "imprevisto"})
	req, _ := body["request"].(map[string]any)
	reqID, _ := req["id"].(string)
	if code, b := do(t, ts, "POST", "/api/v1/refund-requests/"+reqID+"/approve", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("aprovar: %d %v", code, b)
	}

	code, hist := do(t, ts, "GET", "/api/v1/refund-requests/"+reqID+"/history", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("histórico: %d", code)
	}
	events, _ := hist["events"].([]any)
	// pedido aberto → aprovado pelo produtor → em execução → concluído.
	if len(events) != 4 {
		t.Fatalf("esperava 4 transições registradas, veio %d: %v", len(events), events)
	}
	first, _ := events[0].(map[string]any)
	if first["actor_kind"] != "buyer" || first["to_status"] != "pending" {
		t.Fatalf("a primeira linha deveria ser o comprador abrindo: %v", first)
	}
	second, _ := events[1].(map[string]any)
	if second["actor_kind"] != "producer" || second["to_status"] != "approved" {
		t.Fatalf("a segunda deveria ser o produtor aprovando: %v", second)
	}
	last, _ := events[len(events)-1].(map[string]any)
	if last["to_status"] != "completed" {
		t.Fatalf("a última deveria ser a conclusão: %v", last)
	}
}

// TestCancelamentoDoProdutorDeixaTrilha: mesmo a decisão que ninguém pediu precisa
// responder "quem mandou estornar isso, e por quê" meses depois.
func TestCancelamentoDoProdutorDeixaTrilha(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Trilha", "owner@trilha.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 1, "buy@trilha.com", "pix")
	orderID := orderOf(t, ctx, pool, pid, eventID)

	code, body := do(t, ts, "POST", "/api/v1/orders/"+orderID+"/refund", bearer(owner),
		map[string]any{"reason": "casa interditada"})
	if code != http.StatusOK {
		t.Fatalf("cancelamento do produtor: %d %v", code, body)
	}
	if body["request_id"] == nil {
		t.Fatalf("o cancelamento deveria abrir um pedido para a auditoria: %v", body)
	}
	if s := scanStr(t, ctx, pool, pid, `SELECT track FROM refund_requests LIMIT 1`); s != "producer_initiated" {
		t.Fatalf("esperava trilha producer_initiated, veio %s", s)
	}
	if s := scanStr(t, ctx, pool, pid, `SELECT status FROM refund_requests LIMIT 1`); s != "completed" {
		t.Fatalf("esperava completed, veio %s", s)
	}
}

// TestTarifaPeloProdutorEntraNoEstornoDele: com a política mandando o produtor absorver a
// tarifa do gateway, ela entra no estorno dele — devolver o face e ainda pagar a tarifa da
// devolução é escolha comercial, e precisa aparecer no razão.
func TestTarifaPeloProdutorEntraNoEstornoDele(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Tarifa", "owner@tarifa.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	if code, _ := do(t, ts, "PUT", "/api/v1/refund-policy", bearer(owner),
		map[string]any{"withdrawal_window_days": 7, "refund_gateway_fee_bearer": "producer",
			"discretionary_response_hours": 72}); code != http.StatusOK {
		t.Fatalf("gravar política")
	}
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 1, "buy@tarifa.com", "pix")
	orderID := orderOf(t, ctx, pool, pid, eventID)

	face := ledgerSum(t, ctx, pool, pid, "repasse")
	fee := int64(scanInt(t, ctx, pool, pid, `SELECT processing_fee_cents FROM orders WHERE id=$1`, uuid.MustParse(orderID)))
	if fee <= 0 {
		t.Skip("tabela de tarifas sem componente de processamento; nada a provar aqui")
	}

	if code, b := do(t, ts, "POST", "/api/v1/orders/"+orderID+"/refund", bearer(owner),
		map[string]any{"reason": "com tarifa pelo produtor"}); code != http.StatusOK {
		t.Fatalf("estorno: %d %v", code, b)
	}
	if got := ledgerSum(t, ctx, pool, pid, "estorno"); got != -(face + fee) {
		t.Fatalf("esperava estorno de %d (face %d + tarifa %d), veio %d", -(face + fee), face, fee, got)
	}
}
