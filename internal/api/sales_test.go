package api_test

import (
	"context"
	"net/http"
	"testing"
)

// TestBuscaDeVendasPorPessoa: quem liga não sabe o id do pedido — sabe o próprio e-mail, ou
// o CPF, ou o nome. A busca do atendimento é por PESSOA; por id é o caso raro.
func TestBuscaDeVendasPorPessoa(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Busca", "owner@busca.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 2, "cliente@busca.com", "pix")
	orderID := orderOf(t, ctx, pool, pid, eventID)

	// Por e-mail.
	code, body := do(t, ts, "GET", "/api/v1/sales?q=cliente@busca.com", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("busca: %d %v", code, body)
	}
	sales, _ := body["sales"].([]any)
	if len(sales) != 1 {
		t.Fatalf("esperava 1 venda pelo e-mail, veio %v", body["sales"])
	}
	first, _ := sales[0].(map[string]any)
	if first["order_id"] != orderID {
		t.Fatalf("achou a venda errada: %v", first)
	}
	if first["active_tickets"] != float64(2) {
		t.Fatalf("esperava 2 ingressos válidos, veio %v", first["active_tickets"])
	}

	// Por CPF com máscara: o mesmo documento digitado de outro jeito precisa achar.
	cpf := first["buyer_cpf"].(string)
	masked := cpf[:3] + "." + cpf[3:6] + "." + cpf[6:9] + "-" + cpf[9:]
	_, body = do(t, ts, "GET", "/api/v1/sales?q="+masked, bearer(owner), nil)
	if sales, _ := body["sales"].([]any); len(sales) != 1 {
		t.Fatalf("CPF com máscara deveria achar a mesma venda, veio %v", body["sales"])
	}

	// Por id do INGRESSO: é o que a portaria tem na mão.
	code, tk := do(t, ts, "GET", "/api/v1/sales/"+orderID+"/tickets", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("ingressos da venda: %d", code)
	}
	tickets, _ := tk["tickets"].([]any)
	if len(tickets) != 2 {
		t.Fatalf("esperava 2 ingressos, veio %v", tk["tickets"])
	}
	tid := tickets[0].(map[string]any)["id"].(string)
	_, body = do(t, ts, "GET", "/api/v1/sales?q="+tid, bearer(owner), nil)
	if sales, _ := body["sales"].([]any); len(sales) != 1 {
		t.Fatalf("id de ingresso deveria achar a venda, veio %v", body["sales"])
	}

	// Quem atende precisa ver o pedido de estorno vivo ANTES de prometer qualquer coisa.
	buyerHdr := buyer(t, ts, pool, "cliente@busca.com")
	backdateOrder(t, ctx, pool, pid, orderID, 30)
	if code, _ := do(t, ts, "POST", "/api/v1/public/me/orders/"+orderID+"/refund-request", buyerHdr,
		map[string]any{"reason": "imprevisto"}); code != http.StatusAccepted {
		t.Fatalf("abrir pedido")
	}
	_, body = do(t, ts, "GET", "/api/v1/sales?q=cliente@busca.com", bearer(owner), nil)
	sales, _ = body["sales"].([]any)
	first, _ = sales[0].(map[string]any)
	if first["refund_request_status"] != "pending" {
		t.Fatalf("a venda deveria mostrar o pedido vivo, veio %v", first["refund_request_status"])
	}
}

// TestBuscaGlobalDoAdmin: quem escreve para a plataforma não sabe de qual casa comprou —
// que é o caso normal quando a pessoa procura a plataforma, e não o produtor.
func TestBuscaGlobalDoAdmin(t *testing.T) {
	ts, pool := setup(t)
	_, ownerA := createProducer(t, ts, "Casa A", "owner@casaa.com", "senha1234")
	_, ownerB := createProducer(t, ts, "Casa B", "owner@casab.com", "senha1234")
	pidA, pidB := producerID(t, ts, ownerA), producerID(t, ts, ownerB)
	soldEvent(t, ts, pool, ownerA, pidA, 1, "andarilho@x.com", "pix")
	soldEvent(t, ts, pool, ownerB, pidB, 1, "andarilho@x.com", "pix")

	admin := seedAdmin(t, ts, pool, "admin@busca.com", "admin")
	if code, _ := do(t, ts, "GET", "/api/v1/admin/sales", admin, nil); code != http.StatusBadRequest {
		t.Fatalf("busca sem termo deveria ser 400, veio %d", code)
	}
	code, body := do(t, ts, "GET", "/api/v1/admin/sales?q=andarilho@x.com", admin, nil)
	if code != http.StatusOK {
		t.Fatalf("busca global: %d %v", code, body)
	}
	sales, _ := body["sales"].([]any)
	if len(sales) != 2 {
		t.Fatalf("esperava as compras nas duas casas, veio %v", body["sales"])
	}
	casas := map[string]bool{}
	for _, s := range sales {
		casas[s.(map[string]any)["producer_name"].(string)] = true
	}
	if !casas["Casa A"] || !casas["Casa B"] {
		t.Fatalf("a busca precisa dizer de qual casa é cada compra: %v", casas)
	}
}
