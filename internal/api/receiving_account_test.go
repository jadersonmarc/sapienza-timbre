package api_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestPublishRequiresApprovedAccount: abrir venda exige conta de recebimento APROVADA.
// Montar e configurar o evento acontece em qualquer estado — o produtor não fica parado
// enquanto a análise corre —, mas vender sem destino para o dinheiro, não.
func TestPublishRequiresApprovedAccount(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducerWithoutWallet(t, ts, "Casa SemConta", "owner@semconta.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show SemConta", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)

	// Sem conta nenhuma.
	code, body := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil)
	if code != http.StatusConflict || body["needs_wallet"] != true {
		t.Fatalf("sem conta deveria barrar com 409: %d %v", code, body)
	}
	if body["account_status"] != "sem_conta" {
		t.Fatalf("estado deveria ser sem_conta, veio %v", body["account_status"])
	}

	// Conta criada, documentos pendentes: ainda não vende.
	code, created := do(t, ts, "POST", "/api/v1/producer/receiving-account", bearer(owner), map[string]any{
		"legal_name": "Casa SemConta Produções", "tax_id": testCPF("semconta@x.com"),
		"email": "financeiro@semconta.com", "mobile_phone": "31988887777", "birth_date": "1985-03-10",
		"income_cents": 500000, "postal_code": "30140-071", "address": "Rua dos Aimorés",
		"address_number": "100", "province": "Funcionários",
	})
	if code != http.StatusOK {
		t.Fatalf("abrir conta: %d %v", code, created)
	}
	wallet := created["wallet_id"].(string)
	code, body = do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil)
	if code != http.StatusConflict || body["account_status"] != "criada_aguardando_docs" {
		t.Fatalf("com documentos pendentes não deveria vender: %d %v", code, body)
	}

	// Em análise: idem.
	accountWebhook(t, ts, wallet, "PENDING")
	code, body = do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil)
	if code != http.StatusConflict || body["account_status"] != "em_analise" {
		t.Fatalf("em análise não deveria vender: %d %v", code, body)
	}

	// Aprovada: vende.
	accountWebhook(t, ts, wallet, "APPROVED")
	if code, body := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("com conta aprovada deveria publicar: %d %v", code, body)
	}
	code, status := do(t, ts, "GET", "/api/v1/producer/receiving-account", bearer(owner), nil)
	if code != http.StatusOK || status["can_sell"] != true {
		t.Fatalf("status deveria permitir vender: %v", status)
	}
}

// TestAccountReusedAcrossEvents: a conta é do PRODUTOR, não do evento. Do segundo evento em
// diante nada é criado — criar por evento repetiria o documento e o gateway recusaria.
func TestAccountReusedAcrossEvents(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducerWithoutWallet(t, ts, "Casa Reuso", "owner@reuso.com", "senha1234")
	wallet := approveReceivingAccount(t, ts, owner, "owner@reuso.com")

	for i, titulo := range []string{"Primeiro Show", "Segundo Show"} {
		eventID := createEvent(t, ts, owner, titulo, "shows")
		_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
		if code, body := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
			t.Fatalf("evento %d: %d %v", i+1, code, body)
		}
	}
	_, status := do(t, ts, "GET", "/api/v1/producer/receiving-account", bearer(owner), nil)
	if status["wallet_id"] != nil && status["wallet_id"] != wallet {
		t.Fatalf("a carteira do produtor mudou entre eventos: %v", status)
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

// TestPixDoesNotOpenSales: informar chave Pix não abre venda. O repasse agora é dividido na
// própria cobrança, e o destino é a conta de recebimento aprovada — a chave Pix segue no
// cadastro só para resolução manual (divergência, estorno fora do fluxo).
func TestPixDoesNotOpenSales(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducerWithoutWallet(t, ts, "Casa Pix", "owner@pix.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show Pix", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)

	cpf := testCPF("titular@pix.com")
	if code, body := do(t, ts, "POST", "/api/v1/producer/payout-account", bearer(owner), map[string]any{
		"pix_key": cpf, "pix_key_type": "cpf", "holder_name": "Marc Silva", "holder_tax_id": cpf,
	}); code != http.StatusOK {
		t.Fatalf("gravar chave: %d %v", code, body)
	}
	if code, body := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusConflict {
		t.Fatalf("chave Pix não deveria abrir venda: %d %v", code, body)
	}
}

// TestAdminPayoutQueueIgnoresSplit: produtor que recebe pelo split não entra na fila de
// transferência manual — o dinheiro dele já foi na própria cobrança. A fila existe para o
// que sobra: divergência, cancelamento, resolução manual.
func TestAdminPayoutQueueIgnoresSplit(t *testing.T) {
	ts, pool := setup(t)
	admin := seedAdmin(t, ts, pool, "pagador@timbre.com", "super_admin")
	_, owner := createProducer(t, ts, "Casa Fila", "owner@fila.com", "senha1234")

	eventID := createEvent(t, ts, owner, "Show Fila", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 10000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar")
	}
	body := buyViaSession(t, ts, buyer(t, ts, pool, "compra@fila.com"),
		map[string]any{"event_id": eventID, "quantity": 1}, "pix")
	confirmWebhook(t, ts, body["asaas_ref"].(string))

	code, queue := do(t, ts, "GET", "/api/v1/admin/payouts", admin, nil)
	if code != http.StatusOK {
		t.Fatalf("fila: %d %v", code, queue)
	}
	for _, p := range queue["producers"].([]any) {
		if p.(map[string]any)["producer_name"] == "Casa Fila" {
			t.Fatalf("produtor com split não deveria estar na fila manual: %v", p)
		}
	}
}
