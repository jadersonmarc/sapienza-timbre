package payment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// Sonda do sandbox: responde as três perguntas do estorno que só uma devolução de verdade
// responde. Pula sem credencial, como o resto dos testes de integração do repo.
//
//	export ASAAS_SANDBOX_KEY='<chave do sandbox>'
//	go test ./internal/payment/ -run TestSandboxRefundProbe -v
//
// A primeira execução cria a cobrança e para: alguém precisa marcá-la como recebida no
// painel do sandbox (não existe caminho de API documentado aqui para isso, e inventar um
// daria um 404 que parece outra coisa). A segunda execução, com a cobrança paga, faz as
// devoluções e imprime as respostas.
//
//	export ASAAS_PROBE_PAYMENT='<id da cobrança já recebida>'
func TestSandboxRefundProbe(t *testing.T) {
	key := os.Getenv("ASAAS_SANDBOX_KEY")
	if key == "" {
		t.Skip("ASAAS_SANDBOX_KEY não setada — sonda do sandbox pulada")
	}
	base := os.Getenv("ASAAS_BASE_URL")
	if base == "" {
		base = "https://api-sandbox.asaas.com"
	}
	gw := NewAsaas(key, base)
	ctx := context.Background()
	t.Logf("sandbox em %s", gw.BaseURL())

	payID := os.Getenv("ASAAS_PROBE_PAYMENT")
	if payID == "" {
		c, err := gw.CreateCharge(ctx, ChargeRequest{
			OrderID: "probe", Method: MethodPix, AmountCents: 2000,
			BuyerName: "Sonda Timbre", BuyerEmail: "sonda@example.com", BuyerCPF: validCPF(),
			DueDate: time.Now().Add(24 * time.Hour), ExternalReference: "timbre:probe",
		})
		if err != nil {
			t.Fatalf("criar cobrança no sandbox: %v", err)
		}
		t.Logf("cobrança criada: %s", c.AsaasRef)
		t.Logf("PASSO 2 — marque como RECEBIDA no painel do sandbox e rode de novo com:")
		t.Logf("  export ASAAS_PROBE_PAYMENT=%s", c.AsaasRef)
		t.Skip("cobrança criada; aguardando o recebimento para devolver")
	}

	// ── pergunta 1: o estorno aceita chave de idempotência? ──────────────────
	// Duas metades em sequência curta, cada uma com a sua chave. Se as duas passarem e a
	// resposta trouxer ids distintos, dá para conciliar por id — e a janela de eco morre.
	primeira, err := gw.Refund(ctx, RefundRequest{
		AsaasRef: payID, ValueCents: 1000, Description: "timbre:refund:probe-A",
	})
	if err != nil {
		t.Fatalf("primeira devolução parcial: %v", err)
	}
	t.Logf("RESPOSTA 1 — primeira metade: id=%q status=%q valor=%d", primeira.ID, primeira.Status, primeira.ValueCents)

	segunda, err := gw.Refund(ctx, RefundRequest{
		AsaasRef: payID, ValueCents: 1000, Description: "timbre:refund:probe-B",
	})
	if err != nil {
		t.Fatalf("segunda devolução parcial: %v", err)
	}
	t.Logf("RESPOSTA 2 — segunda metade: id=%q status=%q valor=%d", segunda.ID, segunda.Status, segunda.ValueCents)

	switch {
	case primeira.ID == "":
		t.Log("VEREDITO: a resposta NÃO traz id de estorno — conciliar por id está fora, a janela de eco fica")
	case primeira.ID == segunda.ID:
		t.Logf("VEREDITO: as duas devoluções vieram com o MESMO id (%s) — não dá para distinguir, a janela fica", primeira.ID)
	default:
		t.Log("VEREDITO: ids distintos por devolução — dá para conciliar por id e remover a janela de eco")
		t.Log("  falta confirmar se o AVISO (webhook) carrega esse id: ver a linha 'asaas: forma do aviso de estorno' no log do servidor")
	}

	// ── pergunta 2: replay da mesma chave ────────────────────────────────────
	_, err = gw.Refund(ctx, RefundRequest{
		AsaasRef: payID, ValueCents: 1000, Description: "timbre:refund:probe-A",
	})
	switch {
	case err == nil:
		t.Log("RESPOSTA 3 — replay da mesma description foi ACEITO: o gateway não deduplica por ela; a idempotência é só nossa")
	case errors.Is(err, ErrRefundAlreadyExists):
		t.Log("RESPOSTA 3 — replay recusado como 'já estornado': o marcador atual acertou")
	default:
		t.Logf("RESPOSTA 3 — replay recusado por outro motivo (marcador a confirmar): %v", err)
	}

	// ── pergunta 3: marcadores reais de recusa ───────────────────────────────
	// Devolver mais do que sobrou: é a recusa mais fácil de provocar sem depender de saldo.
	_, err = gw.Refund(ctx, RefundRequest{
		AsaasRef: payID, ValueCents: 999999, Description: "timbre:refund:probe-excesso",
	})
	if err == nil {
		t.Log("RESPOSTA 4 — devolver acima do valor foi ACEITO; nada a aprender aqui")
	} else {
		t.Logf("RESPOSTA 4 — recusa por excesso: %v", err)
		t.Logf("  classificado como saldo insuficiente? %v", errors.Is(err, ErrRefundInsufficientFunds))
		t.Logf("  classificado como não estornável?     %v", errors.Is(err, ErrRefundNotRefundable))
		t.Log("  se as duas linhas acima forem false, o texto acima é o marcador que falta em refundRefusalMarkers")
	}

	t.Log("Ao fim: confira no log do SERVIDOR as linhas 'asaas: forma da resposta de estorno' e")
	t.Log("'asaas: forma do aviso de estorno' — elas dizem se o id do estorno viaja no webhook.")
}

// validCPF devolve um CPF com dígitos verificadores corretos, para o cadastro exigido pelo
// gateway. Determinístico e sem relação com pessoa real.
func validCPF() string {
	base := "111444777"
	digits := base
	for _, pos := range []int{9, 10} {
		sum := 0
		for i := range pos {
			sum += int(digits[i]-'0') * (pos + 1 - i)
		}
		d := (sum * 10) % 11
		if d == 10 {
			d = 0
		}
		digits += fmt.Sprintf("%d", d)
	}
	return digits
}
