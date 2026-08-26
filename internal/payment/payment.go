// Package payment define o gateway de pagamento (Asaas, com split no ato da venda) e
// suas implementações: FakeGateway (determinístico, default e usado em testes) e
// AsaasGateway (HTTP real). Interface trocável, no espírito dos drivers da Margot.
package payment

import (
	"context"
	"time"
)

// Method de pagamento.
const (
	MethodPix    = "pix"
	MethodCard   = "credit_card"
	MethodBoleto = "boleto"
	MethodDebit  = "debit_card"
)

// SplitItem é uma fatia do split (destinatário Asaas + valor/percentual).
type SplitItem struct {
	WalletID   string  `json:"wallet_id"`
	Percent    float64 `json:"percent,omitempty"`
	FixedCents int64   `json:"fixed_cents,omitempty"`
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
	Split        []SplitItem // divisão automática no ato da venda
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
	AsaasRef  string
	Type      string
	Confirmed bool // pagamento confirmado/recebido
	Refunded  bool // estorno/contestação (queima os ingressos)
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

// PaymentGateway é a interface com o gateway.
type PaymentGateway interface {
	CreateCharge(ctx context.Context, req ChargeRequest) (Charge, error)
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
