// Package payment define o gateway de pagamento (Asaas) e suas implementações:
// FakeGateway (determinístico, default e usado em testes) e AsaasGateway (HTTP real).
// Interface trocável, no espírito dos drivers da Margot.
//
// NÃO EXISTE SPLIT. A cobrança nasce inteira na conta da plataforma e o repasse ao produtor
// acontece depois da realização do evento — modelo de bilheteria, não de marketplace. Se o
// evento não acontece, o dinheiro precisa estar com quem vai devolver.
package payment

import (
	"context"
	"errors"
	"time"
)

// Method de pagamento.
const (
	MethodPix    = "pix"
	MethodCard   = "credit_card"
	MethodBoleto = "boleto"
	MethodDebit  = "debit_card"
)

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
	// mesma cobrança gera confirmação e estorno, e deduplicar por cobrança descartaria
	// eventos legítimos.
	ID        string
	AsaasRef  string
	Type      string
	Confirmed bool // pagamento confirmado/recebido
	Refunded  bool // estorno/contestação (queima os ingressos)
	// RefundKeys são as descriptions das devoluções que o aviso carrega — a NOSSA chave de
	// volta, quando o gateway a envia. Vazio quando ele não manda: aí a conciliação cai na
	// janela de tempo, que é o que existia antes de haver identidade alguma.
	RefundKeys []string
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
//
// MEDIDO NO SANDBOX (28/08/2026): o Asaas NÃO emite id de estorno. `POST /payments/{id}/
// refund` devolve o objeto da COBRANÇA, e as devoluções vivem em `refunds[]` com os campos
// dateCreated, description, effectiveDate, endToEndIdentifier, refundedSplits, status,
// transactionReceiptUrl e value — sem id.
//
// A identidade de um estorno, portanto, é a `description`: é o único campo que nós
// controlamos, que sobrevive à ida e volta e distingue uma devolução parcial da outra.
type Refund struct {
	// Description é a identidade do estorno — a chave que enviamos e o gateway devolve.
	Description string
	Status      string
	ValueCents  int64
}

// Erros de estorno que o chamador precisa distinguir: cada um leva a um tratamento
// diferente, e um erro genérico transformaria "aguarde" em "não deu".
var (
	// ErrRefundInProgress é outro estorno da MESMA cobrança ainda em processamento. O
	// gateway serializa devoluções por cobrança: é espera, não falha — tentar de novo
	// resolve, e tratar como erro definitivo transformaria um "aguarde" em "não deu".
	ErrRefundInProgress = errors.New("já há um estorno em andamento nesta cobrança")
	// ErrRefundInsufficientFunds é saldo insuficiente NA CONTA DA PLATAFORMA. Deixou de ser
	// o cenário do produtor que já sacou — ele não saca antes do evento —, e por isso deixou
	// de ser rotina: agora é incidente da bilheteria, e o estorno falha em vez de virar
	// dívida de terceiro.
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
	// Fees devolve a tabela de tarifas vigente da conta. O preço do ingresso depende
	// dela, então quem chama precisa tratar falha com o último valor conhecido.
	Fees(ctx context.Context) (Fees, error)
	Configured() bool
}
