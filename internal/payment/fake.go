package payment

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

// FakeGateway é o gateway determinístico usado por default (sem ASAAS_API_KEY) e nos
// testes. Não fala com rede: gera um asaas_ref a partir do order id e um código Pix
// fictício, e normaliza um webhook no nosso formato interno de teste.
//
// O estorno carrega estado porque os modos de falha dele importam: saldo insuficiente é o
// cenário do produtor que já sacou, e é ele que decide se a plataforma cobre a devolução.
// Sem poder ensaiar isso sem rede, o caminho mais caro do estorno ficaria sem teste.
type FakeGateway struct {
	mu sync.Mutex
	// refunds guarda os estornos por cobrança: alimenta o contador de chamadas e a recusa
	// por estorno repetido.
	refunds map[string][]Refund
	// forced é o modo de falha ensaiado por cobrança.
	forced map[string]error
}

// NewFakeGateway constrói o gateway fake.
func NewFakeGateway() *FakeGateway {
	return &FakeGateway{refunds: map[string][]Refund{}, forced: map[string]error{}}
}

// FailRefund faz o próximo estorno da cobrança falhar com err. Só para teste.
func (g *FakeGateway) FailRefund(asaasRef string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.forced[asaasRef] = err
}

// RefundCalls diz quantos estornos a cobrança recebeu. Só para teste: é como se prova que
// o duplo disparo não virou dois estornos.
func (g *FakeGateway) RefundCalls(asaasRef string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.refunds[asaasRef])
}

// Refund devolve um estorno determinístico e registra a chamada. A recusa ensaiada por
// FailRefund vale UMA vez — o teste que quer o cenário de saldo insuficiente seguido de
// sucesso não precisa de outro gateway.
func (g *FakeGateway) Refund(_ context.Context, req RefundRequest) (Refund, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if req.AsaasRef == "" {
		return Refund{}, fmt.Errorf("cobrança obrigatória para estornar")
	}
	if err, ok := g.forced[req.AsaasRef]; ok {
		delete(g.forced, req.AsaasRef)
		return Refund{}, err
	}
	for _, r := range g.refunds[req.AsaasRef] {
		if req.Description != "" && r.Description == req.Description {
			return r, ErrRefundAlreadyExists
		}
	}
	// Espelha o real: sem id, identidade na description (medido no sandbox).
	r := Refund{Status: "DONE", ValueCents: req.ValueCents, Description: req.Description}
	g.refunds[req.AsaasRef] = append(g.refunds[req.AsaasRef], r)
	return r, nil
}

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
	// Campos dos demais fluxos: split e cadastro de subconta.
	ID            string   `json:"id"`
	SplitID       string   `json:"split_id"`
	SplitStatus   string   `json:"split_status"`
	RefusalReason string   `json:"refusal_reason"`
	WalletID      string   `json:"wallet_id"`
	AccountStatus string   `json:"account_status"`
	RefundKeys    []string `json:"refund_keys"`
}

// HandleWebhook decodifica o payload de teste.
func (*FakeGateway) HandleWebhook(_ context.Context, payload []byte) (WebhookEvent, error) {
	var e fakeWebhook
	if err := json.Unmarshal(payload, &e); err != nil {
		return WebhookEvent{}, err
	}
	return WebhookEvent{
		ID: e.ID, AsaasRef: e.AsaasRef, Type: e.Type,
		Confirmed: e.Confirmed, Refunded: e.Refunded,
		SplitID: e.SplitID, SplitStatus: splitStatusFor(e.Type, e.SplitStatus),
		RefusalReason: e.RefusalReason,
		WalletID:      e.WalletID, AccountStatus: e.AccountStatus,
		RefundKeys: e.RefundKeys,
	}, nil
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
