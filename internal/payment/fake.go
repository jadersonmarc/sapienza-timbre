package payment

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// FakeGateway é o gateway determinístico usado por default (sem ASAAS_API_KEY) e nos
// testes. Não fala com rede: gera um asaas_ref a partir do order id e um código Pix
// fictício, e normaliza um webhook no nosso formato interno de teste.
type FakeGateway struct{}

// NewFakeGateway constrói o gateway fake.
func NewFakeGateway() *FakeGateway { return &FakeGateway{} }

// Configured é sempre true (é um stub operável).
func (*FakeGateway) Configured() bool { return true }

// CreateCharge devolve uma cobrança fictícia previsível (asaas_ref = "fake_<order>").
func (*FakeGateway) CreateCharge(_ context.Context, req ChargeRequest) (Charge, error) {
	c := Charge{AsaasRef: "fake_" + req.OrderID, Status: "pending"}
	if req.Method == MethodPix {
		c.PixCode = "00020126FAKE-PIX-" + req.OrderID
	}
	return c, nil
}

// fakeWebhook é o formato de webhook que o FakeGateway entende (para testes).
type fakeWebhook struct {
	AsaasRef  string `json:"asaas_ref"`
	Type      string `json:"type"`
	Confirmed bool   `json:"confirmed"`
	Refunded  bool   `json:"refunded"`
}

// HandleWebhook decodifica o payload de teste.
func (*FakeGateway) HandleWebhook(_ context.Context, payload []byte) (WebhookEvent, error) {
	var e fakeWebhook
	if err := json.Unmarshal(payload, &e); err != nil {
		return WebhookEvent{}, err
	}
	return WebhookEvent{AsaasRef: e.AsaasRef, Type: e.Type, Confirmed: e.Confirmed, Refunded: e.Refunded}, nil
}

// CreateAccount devolve uma carteira determinística a partir do documento — sem rede, e
// estável entre execuções, para o teste poder afirmar qual carteira ficou no produtor.
func (*FakeGateway) CreateAccount(_ context.Context, in AccountInput) (Account, error) {
	if in.TaxID == "" {
		return Account{}, fmt.Errorf("documento obrigatório para abrir a conta de recebimento")
	}
	sum := sha256.Sum256([]byte("wallet:" + in.TaxID))
	return Account{WalletID: uuid.NewSHA1(uuid.Nil, sum[:]).String()}, nil
}

// Fees devolve uma tabela determinística para os testes: valores plausíveis, sem rede.
func (*FakeGateway) Fees(_ context.Context) (Fees, error) {
	return Fees{
		Pix:       MethodFee{Pct: 0.99},
		Boleto:    MethodFee{FixedCents: 199},
		DebitCard: MethodFee{Pct: 1.89, FixedCents: 35},
		CreditCard: []CreditTier{
			{MinInstallments: 1, MaxInstallments: 1, Pct: 2.99, FixedCents: 49},
			{MinInstallments: 2, MaxInstallments: 6, Pct: 3.49, FixedCents: 49},
			{MinInstallments: 7, MaxInstallments: 21, Pct: 3.99, FixedCents: 49},
		},
		Raw: []byte(`{"fake":true}`),
	}, nil
}

// AccountDocuments simula uma pendência de documento com link de onboarding.
func (*FakeGateway) AccountDocuments(_ context.Context, walletID string) (AccountDocuments, error) {
	return AccountDocuments{Items: []AccountDocument{{
		ID: "doc_" + walletID, Type: "IDENTIFICATION", Status: "PENDING",
		OnboardingURL: "https://sandbox.asaas.com/onboarding/" + walletID,
	}}}, nil
}
