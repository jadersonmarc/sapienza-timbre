package payment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// refundSpy sobe um gateway de teste que devolve status/corpo escolhidos no estorno, e
// captura o payload enviado.
func refundSpy(t *testing.T, status int, body string, capture *map[string]any, path *string) *AsaasGateway {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if capture != nil {
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			*capture = m
		}
		if path != nil {
			*path = r.URL.Path
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewAsaas("key", srv.URL)
}

// TestRefundIntegralOmitsValue: estorno integral NÃO manda "value". Mandar zero seria
// pedir uma devolução de zero — o campo ausente é o que significa "tudo".
func TestRefundIntegralOmitsValue(t *testing.T) {
	var sent map[string]any
	var path string
	gw := refundSpy(t, http.StatusOK,
		`{"id":"pay_1","status":"RECEIVED","value":56.00,
		  "refunds":[{"description":"timbre:refund:abc","status":"DONE","value":56.00}]}`, &sent, &path)

	out, err := gw.Refund(context.Background(), RefundRequest{AsaasRef: "pay_1", Description: "timbre:refund:abc"})
	if err != nil {
		t.Fatalf("estorno: %v", err)
	}
	if path != "/v3/payments/pay_1/refund" {
		t.Fatalf("caminho inesperado: %s", path)
	}
	if _, ok := sent["value"]; ok {
		t.Fatalf("integral não pode enviar value, veio %v", sent["value"])
	}
	if sent["description"] != "timbre:refund:abc" {
		t.Fatalf("a chave de idempotência precisa acompanhar o estorno, veio %v", sent["description"])
	}
	// O gateway NÃO emite id de estorno (medido no sandbox): a devolução é uma entrada de
	// refunds[] e a identidade é a description que enviamos. Ler o `id` do topo daria o id
	// da COBRANÇA — foi o que este cliente fez até medirmos.
	if out.Description != "timbre:refund:abc" {
		t.Fatalf("a identidade do estorno é a description, veio %q", out.Description)
	}
	if out.Status != "DONE" {
		t.Fatalf("status deveria vir da entrada de refunds[], veio %q", out.Status)
	}
	if out.ValueCents != 5600 {
		t.Fatalf("esperava 5600 centavos, veio %d", out.ValueCents)
	}
}

// TestRefundPartialSendsValueInReais: o gateway trabalha em reais; centavo enviado como
// centavo devolveria cem vezes o valor.
func TestRefundPartialSendsValueInReais(t *testing.T) {
	var sent map[string]any
	gw := refundSpy(t, http.StatusOK,
		`{"id":"pay_1","status":"RECEIVED","value":56.00,
		  "refunds":[{"description":"parcial","status":"DONE","value":12.34}]}`, &sent, nil)

	out, err := gw.Refund(context.Background(), RefundRequest{AsaasRef: "pay_1", ValueCents: 1234, Description: "parcial"})
	if err != nil {
		t.Fatalf("estorno parcial: %v", err)
	}
	if sent["value"] != 12.34 {
		t.Fatalf("esperava value 12.34, veio %v", sent["value"])
	}
	// O valor vem da ENTRADA do estorno, não do topo: o topo é o valor da cobrança (5600).
	if out.ValueCents != 1234 {
		t.Fatalf("esperava 1234 centavos (valor do estorno, não da cobrança), veio %d", out.ValueCents)
	}
}

// TestRefundClassifiesRefusals: os três motivos de recusa levam a tratamentos diferentes —
// saldo insuficiente faz a plataforma cobrir, estorno repetido é sucesso disfarçado, e
// cobrança não estornável é erro de quem chamou. Confundi-los custa dinheiro.
func TestRefundClassifiesRefusals(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{"saldo", `{"errors":[{"description":"Saldo insuficiente para o estorno"}]}`, ErrRefundInsufficientFunds},
		{"repetido", `{"errors":[{"description":"Cobrança já estornada"}]}`, ErrRefundAlreadyExists},
		{"não estornável", `{"errors":[{"description":"Esta cobrança não pode ser estornada"}]}`, ErrRefundNotRefundable},
		// MEDIDO no sandbox: o gateway serializa devoluções por cobrança e recusa a
		// concorrente com esta frase. É espera, não falha.
		{"em andamento", `{"errors":[{"code":"invalid_object","description":"O estorno dessa cobrança já está em andamento."}]}`, ErrRefundInProgress},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gw := refundSpy(t, http.StatusBadRequest, c.body, nil, nil)
			_, err := gw.Refund(context.Background(), RefundRequest{AsaasRef: "pay_1"})
			if !errors.Is(err, c.want) {
				t.Fatalf("esperava %v, veio %v", c.want, err)
			}
		})
	}
}

// TestRefundUnknownRefusalStaysUnknown: motivo que não sabemos ler NÃO pode virar "saldo
// insuficiente" por descarte — isso faria a plataforma cobrir uma devolução que o gateway
// recusou por outro motivo, sem ninguém perceber.
func TestRefundUnknownRefusalStaysUnknown(t *testing.T) {
	gw := refundSpy(t, http.StatusBadRequest, `{"errors":[{"description":"Erro desconhecido"}]}`, nil, nil)
	_, err := gw.Refund(context.Background(), RefundRequest{AsaasRef: "pay_1"})
	if err == nil {
		t.Fatal("esperava erro")
	}
	for _, known := range []error{ErrRefundInsufficientFunds, ErrRefundAlreadyExists, ErrRefundNotRefundable, ErrRefundInProgress} {
		if errors.Is(err, known) {
			t.Fatalf("recusa desconhecida foi classificada como %v", known)
		}
	}
}

// TestFakeRefundIsIdempotentByDescription: o fake precisa reproduzir o estorno repetido,
// senão o caminho de idempotência do checkout nunca é exercitado sem rede.
func TestFakeRefundIsIdempotentByDescription(t *testing.T) {
	gw := NewFakeGateway()
	req := RefundRequest{AsaasRef: "fake_1", ValueCents: 100, Description: "timbre:refund:x"}
	if _, err := gw.Refund(context.Background(), req); err != nil {
		t.Fatalf("primeiro estorno: %v", err)
	}
	if _, err := gw.Refund(context.Background(), req); !errors.Is(err, ErrRefundAlreadyExists) {
		t.Fatalf("segundo estorno com a mesma chave: esperava ErrRefundAlreadyExists, veio %v", err)
	}
	if n := gw.RefundCalls("fake_1"); n != 1 {
		t.Fatalf("esperava 1 estorno registrado, veio %d", n)
	}
}
