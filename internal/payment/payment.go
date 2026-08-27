// Package payment define o gateway de pagamento (Asaas, com split no ato da venda) e
// suas implementações: FakeGateway (determinístico, default e usado em testes) e
// AsaasGateway (HTTP real). Interface trocável, no espírito dos drivers da Margot.
package payment

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Method de pagamento.
const (
	MethodPix    = "pix"
	MethodCard   = "credit_card"
	MethodBoleto = "boleto"
	MethodDebit  = "debit_card"
)

// SplitItem é uma fatia do split: quanto vai para a carteira de quem recebe.
//
// SEMPRE valor fixo, nunca percentual. O gateway calcula percentual sobre o LÍQUIDO, o que
// faria o produtor absorver parte da tarifa — e a promessa do modelo é face limpo.
type SplitItem struct {
	WalletID   string `json:"wallet_id"`
	FixedCents int64  `json:"fixed_cents"`
	// ExternalReference identifica o repasse do nosso lado no webhook de liquidação.
	ExternalReference string `json:"external_reference,omitempty"`
}

// ChargeRequest descreve uma cobrança (Pix ou cartão com parcelamento).
type ChargeRequest struct {
	OrderID      string
	Method       string // "pix" | "credit_card"
	AmountCents  int64
	Installments int
	BuyerName    string
	BuyerEmail   string
	BuyerCPF     string
	BuyerPhone   string
	DueDate      time.Time
	// Split: uma fatia por recebedor. O emissor (Timbre) NÃO se declara — recebe
	// automaticamente o líquido não distribuído, e declarar a própria carteira é exceção
	// na API.
	Split []SplitItem
	// ExternalReference amarra a cobrança ao pedido do nosso lado.
	ExternalReference string
	// Card e Holder existem no cartão transparente: o comprador digita na NOSSA tela e os
	// dados seguem para o gateway nesta requisição. Não são persistidos em lugar nenhum e
	// nunca entram em log.
	Card     *CardData
	Holder   *HolderData
	RemoteIP string // exigido pelo gateway para antifraude no cartão
}

// CardData são os dados do cartão. Vivem apenas durante a requisição.
type CardData struct {
	HolderName  string
	Number      string
	ExpiryMonth string
	ExpiryYear  string
	CCV         string
}

// HolderData é o titular do cartão, como o gateway exige para antifraude.
type HolderData struct {
	Name          string
	Email         string
	TaxID         string
	PostalCode    string
	AddressNumber string
	Phone         string
}

// Charge é a cobrança criada.
type Charge struct {
	AsaasRef string
	Status   string
	PixCode  string // "copia e cola" do Pix (quando method = pix)
	// InvoiceURL é a página de pagamento do gateway. É por ela que o cartão é pago: os
	// dados do cartão nunca passam por aqui, então o comprador termina a compra no
	// ambiente do gateway.
	InvoiceURL string
}

// WebhookEvent é o evento decodificado do webhook do gateway. A idempotência é do
// chamador (o checkout ignora um asaas_ref já confirmado/estornado).
type WebhookEvent struct {
	// ID é o identificador DO EVENTO. A idempotência é por ele, não pela cobrança: uma
	// mesma cobrança gera confirmação, liquidação de split e estorno, e deduplicar por
	// cobrança descartaria eventos legítimos.
	ID        string
	AsaasRef  string
	Type      string
	Confirmed bool // pagamento confirmado/recebido
	Refunded  bool // estorno/contestação (queima os ingressos)

	// ── split ────────────────────────────────────────────────────────────────
	// SplitID identifica QUAL split do pagamento originou o evento (uma cobrança pode
	// ter vários recebedores).
	SplitID       string
	SplitStatus   string
	RefusalReason string

	// ── conta (subconta do produtor) ─────────────────────────────────────────
	// WalletID identifica a subconta quando o evento é de cadastro, não de cobrança.
	WalletID                string
	AccountStatus           string
	CommercialInfoExpiresAt *time.Time
}

// Kinds de evento que o webhook distingue. Cobrança e cadastro de subconta são fluxos
// diferentes, com tratamento diferente.
const (
	EventKindPayment = "payment"
	EventKindSplit   = "split"
	EventKindAccount = "account"
)

// Kind classifica o evento pelo tipo declarado pelo gateway.
func (e WebhookEvent) Kind() string {
	switch {
	case strings.HasPrefix(e.Type, "PAYMENT_SPLIT_"):
		return EventKindSplit
	case strings.HasPrefix(e.Type, "ACCOUNT_STATUS"):
		return EventKindAccount
	default:
		return EventKindPayment
	}
}

// AccountInput são os dados que o gateway exige para abrir a conta de recebimento do
// produtor (subconta). É KYC: o gateway precisa saber de quem é o dinheiro.
type AccountInput struct {
	Name          string // nome/razão social
	Email         string
	TaxID         string // CPF ou CNPJ (só dígitos)
	BirthDate     string // AAAA-MM-DD (pessoa física)
	CompanyType   string // vazio = pessoa física; MEI|LIMITED|INDIVIDUAL|ASSOCIATION
	MobilePhone   string
	IncomeCents   int64  // renda/faturamento mensal declarado
	PostalCode    string // CEP (só dígitos)
	Address       string
	AddressNumber string
	Province      string // bairro
}

// Account é a conta de recebimento criada no gateway. Guardamos só o WalletID: é o que o
// split precisa. A chave de API da subconta é devolvida pelo gateway UMA ÚNICA VEZ e é
// descartada de propósito — não é necessária neste desenho, e custodiar credencial de
// terceiro é responsabilidade sem contrapartida.
type Account struct {
	WalletID string
	// CommercialInfoExpired e CommercialInfoExpiresAt vêm de commercialInfoExpiration: a
	// confirmação anual de dados comerciais é exigência regulatória, e sem ela a subconta
	// perde o uso da API.
	CommercialInfoExpired   bool
	CommercialInfoExpiresAt *time.Time
}

// AccountDocuments são as pendências de documentação de uma subconta e por onde resolvê-las.
type AccountDocuments struct {
	Items []AccountDocument
}

// AccountDocument é uma pendência: o tipo e o link para o titular resolver.
type AccountDocument struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	Status        string `json:"status"`
	OnboardingURL string `json:"onboarding_url"`
}

// RefundRequest descreve a devolução de uma cobrança já paga.
type RefundRequest struct {
	// AsaasRef é a cobrança original.
	AsaasRef string
	// ValueCents zero estorna o valor integral; maior que zero, estorna parcial. Uma
	// cobrança pode receber vários parciais.
	ValueCents int64
	// Description acompanha o estorno no extrato do comprador e carrega a nossa chave de
	// idempotência: o gateway não expõe cabeçalho próprio para isso, então a garantia de
	// "um estorno só" é nossa (índice único em refunds), e este campo é o que permite
	// reconhecer, do lado de lá, qual operação originou a devolução.
	Description string
}

// Refund é o estorno criado no gateway.
type Refund struct {
	// ID identifica o ESTORNO, não a cobrança — é por ele que o webhook de devolução é
	// reconciliado com a operação que o originou.
	ID          string
	Status      string
	ValueCents  int64
	Description string
}

// Erros de estorno que o chamador precisa distinguir. Saldo insuficiente não é falha de
// programação nem indisponibilidade: é o cenário esperado de produtor que já sacou, e
// decide se a plataforma cobre a devolução.
var (
	ErrRefundInsufficientFunds = errors.New("saldo insuficiente para o estorno")
	ErrRefundNotRefundable     = errors.New("cobrança não estornável")
	ErrRefundAlreadyExists     = errors.New("estorno já existente")
)

// PaymentGateway é a interface com o gateway.
type PaymentGateway interface {
	CreateCharge(ctx context.Context, req ChargeRequest) (Charge, error)
	// Refund devolve o valor ao comprador na cobrança original. Devolve
	// ErrRefundInsufficientFunds, ErrRefundNotRefundable ou ErrRefundAlreadyExists quando
	// o gateway recusa por um desses motivos — cada um leva a um tratamento diferente.
	Refund(ctx context.Context, req RefundRequest) (Refund, error)
	HandleWebhook(ctx context.Context, payload []byte) (WebhookEvent, error)
	// CreateAccount abre a conta de recebimento do produtor (subconta da plataforma).
	CreateAccount(ctx context.Context, in AccountInput) (Account, error)
	// Fees devolve a tabela de tarifas vigente da conta. O preço do ingresso depende
	// dela, então quem chama precisa tratar falha com o último valor conhecido.
	Fees(ctx context.Context) (Fees, error)
	// AccountDocuments lista as pendências de documentação da subconta recém-criada e o
	// link de onboarding de cada uma.
	AccountDocuments(ctx context.Context, walletID string) (AccountDocuments, error)
	Configured() bool
}
