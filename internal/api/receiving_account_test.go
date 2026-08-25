package api_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestPublishRequiresReceivingAccount: publicar é abrir a venda. Sem conta de recebimento a
// cobrança sairia sem divisão — a compra funcionaria e o dinheiro do produtor ficaria na
// plataforma, sem erro nenhum. O guarda troca esse silêncio por um recado antes da venda.
func TestPublishRequiresReceivingAccount(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducerWithoutWallet(t, ts, "Casa SemConta", "owner@semconta.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show SemConta", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)

	code, body := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil)
	if code != http.StatusConflict {
		t.Fatalf("esperava 409 sem conta de recebimento, veio %d %v", code, body)
	}
	if body["needs_wallet"] != true {
		t.Fatalf("a resposta deveria dizer o que falta: %v", body)
	}

	// O painel consegue perguntar antes de esbarrar no guarda.
	code, status := do(t, ts, "GET", "/api/v1/producer/receiving-account", bearer(owner), nil)
	if code != http.StatusOK || status["configured"] != false {
		t.Fatalf("status deveria dizer não configurado, veio %d %v", code, status)
	}

	// Configurada, publica.
	if code, body := do(t, ts, "POST", "/api/v1/producer/receiving-account", bearer(owner),
		map[string]any{"wallet_id": uuid.NewString()}); code != http.StatusOK {
		t.Fatalf("configurar: %d %v", code, body)
	}
	if code, body := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("com conta de recebimento deveria publicar, veio %d %v", code, body)
	}
	code, status = do(t, ts, "GET", "/api/v1/producer/receiving-account", bearer(owner), nil)
	if status["configured"] != true {
		t.Fatalf("status deveria dizer configurado, veio %v", status)
	}
}

// TestOpenReceivingAccount: quem não tem conta no gateway abre pela plataforma, mandando os
// dados que o gateway exige. Dado faltando é recusado com o que falta, em português.
func TestOpenReceivingAccount(t *testing.T) {
	ts, pool := setup(t)
	pid, owner := createProducerWithoutWallet(t, ts, "Casa Abre", "owner@abre.com", "senha1234")

	completo := map[string]any{
		"legal_name": "Casa Abre Produções", "tax_id": "11144477735",
		"email": "financeiro@abre.com", "mobile_phone": "31988887777",
		"birth_date": "1985-03-10", "income_cents": 500000,
		"postal_code": "30140-071", "address": "Rua dos Aimorés",
		"address_number": "100", "province": "Funcionários",
	}
	faltando := map[string]map[string]any{
		"sem documento":      {"tax_id": ""},
		"documento inválido": {"tax_id": "11144477734"},
		"sem nascimento":     {"birth_date": ""},
		"celular curto":      {"mobile_phone": "31988"},
		"sem faturamento":    {"income_cents": 0},
		"CEP incompleto":     {"postal_code": "301"},
		"sem bairro":         {"province": ""},
	}
	for nome, patch := range faltando {
		req := map[string]any{}
		for k, v := range completo {
			req[k] = v
		}
		for k, v := range patch {
			req[k] = v
		}
		if code, body := do(t, ts, "POST", "/api/v1/producer/receiving-account", bearer(owner), req); code != http.StatusBadRequest {
			t.Fatalf("%s: esperava 400, veio %d %v", nome, code, body)
		}
	}

	code, body := do(t, ts, "POST", "/api/v1/producer/receiving-account", bearer(owner), completo)
	if code != http.StatusOK {
		t.Fatalf("abrir conta: %d %v", code, body)
	}
	if body["created"] != true || body["wallet_id"] == "" {
		t.Fatalf("deveria ter aberto a conta e devolvido a carteira: %v", body)
	}
	var saved string
	if err := pool.QueryRow(t.Context(), `SELECT COALESCE(asaas_wallet_id,'') FROM producers WHERE id=$1`,
		uuid.MustParse(pid)).Scan(&saved); err != nil {
		t.Fatalf("ler produtor: %v", err)
	}
	if saved != body["wallet_id"] {
		t.Fatalf("a carteira deveria ficar gravada no produtor, veio %q", saved)
	}
}

// TestReceivingAccountIsPerProducer: a carteira de um produtor não vaza para outro — cada
// um recebe o seu.
func TestReceivingAccountIsPerProducer(t *testing.T) {
	ts, _ := setup(t)
	_, umOwner := createProducerWithoutWallet(t, ts, "Casa Um", "owner@um.com", "senha1234")
	_, doisOwner := createProducerWithoutWallet(t, ts, "Casa Dois", "owner@dois.com", "senha1234")

	if code, _ := do(t, ts, "POST", "/api/v1/producer/receiving-account", bearer(umOwner),
		map[string]any{"wallet_id": uuid.NewString()}); code != http.StatusOK {
		t.Fatalf("configurar casa um")
	}
	_, status := do(t, ts, "GET", "/api/v1/producer/receiving-account", bearer(doisOwner), nil)
	if status["configured"] != false {
		t.Fatalf("a casa dois não deveria herdar a conta da casa um: %v", status)
	}
}
