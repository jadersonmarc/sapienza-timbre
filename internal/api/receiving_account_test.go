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

// TestPayoutByPix: o caminho de lançamento. Sem subconta no gateway, a chave Pix é o que
// libera a publicação — a plataforma recebe e transfere depois.
func TestPayoutByPix(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducerWithoutWallet(t, ts, "Casa Pix", "owner@pix.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show Pix", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)

	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusConflict {
		t.Fatalf("sem destino do dinheiro não deveria publicar")
	}

	cpf := testCPF("titular@pix.com")
	// A chave precisa ser do titular informado: repassar para chave de terceiro embaralha
	// de quem é o dinheiro recebido.
	if code, body := do(t, ts, "POST", "/api/v1/producer/payout-account", bearer(owner), map[string]any{
		"pix_key": testCPF("outro@pix.com"), "pix_key_type": "cpf",
		"holder_name": "Marc Silva", "holder_tax_id": cpf,
	}); code != http.StatusBadRequest {
		t.Fatalf("chave de outro titular deveria ser recusada, veio %d %v", code, body)
	}
	// Tipo de chave errado para o formato também.
	if code, _ := do(t, ts, "POST", "/api/v1/producer/payout-account", bearer(owner), map[string]any{
		"pix_key": "não-é-email", "pix_key_type": "email",
		"holder_name": "Marc Silva", "holder_tax_id": cpf,
	}); code != http.StatusBadRequest {
		t.Fatalf("chave de e-mail inválida deveria ser recusada")
	}

	if code, body := do(t, ts, "POST", "/api/v1/producer/payout-account", bearer(owner), map[string]any{
		"pix_key": cpf, "pix_key_type": "cpf",
		"holder_name": "Marc Silva", "holder_tax_id": cpf,
	}); code != http.StatusOK {
		t.Fatalf("chave válida: %d %v", code, body)
	}
	if code, body := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("com chave Pix deveria publicar, veio %d %v", code, body)
	}

	// O painel sabe COMO o produtor recebe, e a chave volta mascarada.
	code, status := do(t, ts, "GET", "/api/v1/producer/payout-account", bearer(owner), nil)
	if code != http.StatusOK || status["mode"] != "payout" {
		t.Fatalf("modo deveria ser payout, veio %d %v", code, status)
	}
	if key, _ := status["pix_key"].(string); key == cpf || key == "" {
		t.Fatalf("a chave deveria voltar mascarada, veio %q", key)
	}
}

// TestAdminPayoutQueue: com o dinheiro centralizado, a plataforma precisa saber quanto deve
// a quem e para onde mandar. Quem tem venda mas não cadastrou destino aparece marcado —
// esse é o caso que precisa de cobrança, não de transferência.
func TestAdminPayoutQueue(t *testing.T) {
	ts, pool := setup(t)
	admin := seedAdmin(t, ts, pool, "pagador@timbre.com", "super_admin")
	_, owner := createProducerWithoutWallet(t, ts, "Casa Fila", "owner@fila.com", "senha1234")

	cpf := testCPF("fila@fila.com")
	if code, _ := do(t, ts, "POST", "/api/v1/producer/payout-account", bearer(owner), map[string]any{
		"pix_key": cpf, "pix_key_type": "cpf", "holder_name": "Marc Silva", "holder_tax_id": cpf,
	}); code != http.StatusOK {
		t.Fatalf("configurar repasse")
	}
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
	producers, _ := queue["producers"].([]any)
	var found map[string]any
	for _, p := range producers {
		row := p.(map[string]any)
		if row["producer_name"] == "Casa Fila" {
			found = row
		}
	}
	if found == nil {
		t.Fatalf("produtor com venda deveria aparecer na fila: %v", queue)
	}
	// O repasse libera D+2 depois do evento, então logo após a venda o valor aparece como
	// "a liberar" — e é isso que a fila precisa mostrar para o trabalho não sumir.
	if found["upcoming_cents"].(float64) <= 0 {
		t.Fatalf("deveria haver valor a liberar: %v", found)
	}
	if found["net_due_cents"].(float64) != 0 {
		t.Fatalf("nada deveria estar liberado ainda: %v", found)
	}
	if found["pix_key"] != cpf {
		t.Fatalf("a fila precisa trazer a chave para transferir, veio %v", found["pix_key"])
	}
	if found["blocked"] == true {
		t.Fatalf("com chave cadastrada não deveria estar bloqueado")
	}

	// Marcar pago exige comprovante.
	if code, _ := do(t, ts, "POST", "/api/v1/admin/producers/"+found["producer_id"].(string)+"/payouts/mark-paid",
		admin, map[string]any{"payout_id": uuid.NewString()}); code != http.StatusBadRequest {
		t.Fatalf("sem referência deveria recusar")
	}
}
