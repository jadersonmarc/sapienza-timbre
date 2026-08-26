package payment

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// asaasSpy sobe um gateway de teste no lugar da API e devolve o payload que recebeu.
func asaasSpy(t *testing.T, capture *map[string]any) *AsaasGateway {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/v3/customers" {
			_, _ = w.Write([]byte(`{"id":"cus_1"}`))
			return
		}
		// Só a criação da cobrança interessa: o Pix ainda busca o QR depois, e capturar
		// essa segunda chamada apagaria o payload que queremos conferir.
		if r.Method == http.MethodPost && r.URL.Path == "/v3/payments" {
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			*capture = m
		}
		_, _ = w.Write([]byte(`{"id":"pay_1","status":"PENDING","invoiceUrl":"https://x/y","payload":"pix-copia-e-cola"}`))
	}))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	g := NewAsaas("chave", base.String())
	return g
}

// TestSplitUsaValorFixo: o repasse é sempre valor FIXO. Percentual no gateway incide sobre o
// líquido, o que faria o produtor absorver parte da tarifa — e a promessa é face limpo.
func TestSplitUsaValorFixo(t *testing.T) {
	var got map[string]any
	g := asaasSpy(t, &got)
	_, err := g.CreateCharge(context.Background(), ChargeRequest{
		OrderID: "o1", Method: MethodPix, AmountCents: 11110, BuyerCPF: "11144477735",
		ExternalReference: "timbre:order:o1",
		Split:             []SplitItem{{WalletID: "w_produtor", FixedCents: 10000, ExternalReference: "timbre:repasse:o1"}},
	})
	if err != nil {
		t.Fatalf("cobrança: %v", err)
	}
	splits, ok := got["splits"].([]any)
	if !ok || len(splits) != 1 {
		t.Fatalf("esperava um split em 'splits', veio %v", got["splits"])
	}
	s := splits[0].(map[string]any)
	if s["fixedValue"] != "100.00" {
		t.Fatalf("o repasse deveria ir como valor fixo do face, veio %v", s["fixedValue"])
	}
	if _, tem := s["percentualValue"]; tem {
		t.Fatalf("percentual faria o produtor absorver a tarifa: %v", s)
	}
	if s["walletId"] != "w_produtor" {
		t.Fatalf("carteira errada: %v", s)
	}
	if got["externalReference"] != "timbre:order:o1" {
		t.Fatalf("cobrança sem referência do pedido: %v", got["externalReference"])
	}
}

// TestSplitParceladoUsaTotal: com fixedValue o produtor receberia o face A CADA parcela.
func TestSplitParceladoUsaTotal(t *testing.T) {
	var got map[string]any
	g := asaasSpy(t, &got)
	_, err := g.CreateCharge(context.Background(), ChargeRequest{
		OrderID: "o2", Method: MethodCard, AmountCents: 120000, Installments: 6,
		Split: []SplitItem{{WalletID: "w_produtor", FixedCents: 100000}},
	})
	if err != nil {
		t.Fatalf("cobrança: %v", err)
	}
	s := got["splits"].([]any)[0].(map[string]any)
	if s["totalFixedValue"] != "1000.00" {
		t.Fatalf("parcelado deveria usar totalFixedValue, veio %v", s)
	}
	if _, tem := s["fixedValue"]; tem {
		t.Fatalf("fixedValue no parcelado multiplicaria o repasse por parcela: %v", s)
	}
	if got["installmentCount"] != float64(6) || got["totalValue"] != "1200.00" {
		t.Fatalf("parcelamento inconsistente: %v", got)
	}
}

// TestSemSplitNaoEnviaCampo: cortesia e venda sem carteira não mandam splits — enviar
// vazio/nulo num PUT desativaria a configuração existente.
func TestSemSplitNaoEnviaCampo(t *testing.T) {
	var got map[string]any
	g := asaasSpy(t, &got)
	if _, err := g.CreateCharge(context.Background(), ChargeRequest{
		OrderID: "o3", Method: MethodPix, AmountCents: 5000,
	}); err != nil {
		t.Fatalf("cobrança: %v", err)
	}
	if _, tem := got["splits"]; tem {
		t.Fatalf("sem recebedor não deveria enviar splits: %v", got)
	}
}
