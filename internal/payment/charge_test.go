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
	return NewAsaas("chave", base.String())
}

// TestCobrancaNaoTemRecebedorSecundario: o valor INTEIRO cai na conta da bilheteria.
//
// Este é o teste que guarda a decisão: enquanto houve split, o face saía na própria
// cobrança para a conta do produtor — e o dinheiro de um evento que não aconteceu já não
// estava com quem teria de devolvê-lo. Um campo `splits` reaparecendo aqui é a volta
// silenciosa daquele modelo.
func TestCobrancaNaoTemRecebedorSecundario(t *testing.T) {
	var got map[string]any
	g := asaasSpy(t, &got)
	if _, err := g.CreateCharge(context.Background(), ChargeRequest{
		OrderID: "o1", Method: MethodPix, AmountCents: 11110, BuyerCPF: "11144477735",
		ExternalReference: "timbre:order:o1",
	}); err != nil {
		t.Fatalf("cobrança: %v", err)
	}
	if _, tem := got["splits"]; tem {
		t.Fatalf("a cobrança não pode ter recebedor secundário: %v", got)
	}
	if got["value"] != "111.10" {
		t.Fatalf("a cobrança deveria ser pelo total: %v", got["value"])
	}
	if got["externalReference"] != "timbre:order:o1" {
		t.Fatalf("cobrança sem referência do pedido: %v", got["externalReference"])
	}
}

// TestParceladoMandaTotalEContagem: dividir aqui perderia centavos na divisão inteira —
// o gateway sabe distribuir a sobra entre as parcelas.
func TestParceladoMandaTotalEContagem(t *testing.T) {
	var got map[string]any
	g := asaasSpy(t, &got)
	if _, err := g.CreateCharge(context.Background(), ChargeRequest{
		OrderID: "o2", Method: MethodCard, AmountCents: 120000, Installments: 6,
	}); err != nil {
		t.Fatalf("cobrança: %v", err)
	}
	if got["installmentCount"] != float64(6) || got["totalValue"] != "1200.00" {
		t.Fatalf("parcelamento inconsistente: %v", got)
	}
	if _, tem := got["splits"]; tem {
		t.Fatalf("parcelado também não tem recebedor secundário: %v", got)
	}
}
