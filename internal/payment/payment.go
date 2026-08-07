// Package payment define o gateway de pagamento (Asaas, com split no ato da venda).
// Interface trocável; a implementação real chega na Etapa 1.4. Aqui só o seam.
package payment

import (
	"context"
	"errors"
)

// ErrNotImplemented é devolvido pelo stub até a Etapa 1.4 ligar o Asaas.
var ErrNotImplemented = errors.New("payment: gateway não implementado (Etapa 1.4)")

// ChargeRequest descreve uma cobrança (Pix ou cartão com parcelamento).
type ChargeRequest struct {
	OrderID      string
	Method       string // "pix" | "credit_card"
	AmountCents  int64
	Installments int
	Split        []SplitItem // divisão automática no ato da venda
}

// SplitItem é uma fatia do split (destinatário Asaas + valor/percentual).
type SplitItem struct {
	WalletID   string
	Percent    float64
	FixedCents int64
}

// Charge é a cobrança criada.
type Charge struct {
	AsaasRef string
	Status   string
	PixCode  string // quando method = pix
}

// WebhookEvent é o evento decodificado do webhook do gateway (com idempotência a
// cargo do chamador).
type WebhookEvent struct {
	AsaasRef string
	Type     string
	Status   string
}

// PaymentGateway é a interface com o gateway.
type PaymentGateway interface {
	CreateCharge(ctx context.Context, req ChargeRequest) (Charge, error)
	HandleWebhook(ctx context.Context, payload []byte) (WebhookEvent, error)
}

// AsaasGateway é o stub do gateway Asaas. Configured() é false até haver credenciais;
// os métodos devolvem ErrNotImplemented nesta etapa.
type AsaasGateway struct {
	apiKey string
}

// NewAsaasStub constrói o stub (apiKey pode ser vazio nesta etapa).
func NewAsaasStub(apiKey string) *AsaasGateway { return &AsaasGateway{apiKey: apiKey} }

// Configured diz se há credenciais para operar.
func (g *AsaasGateway) Configured() bool { return g.apiKey != "" }

func (g *AsaasGateway) CreateCharge(context.Context, ChargeRequest) (Charge, error) {
	return Charge{}, ErrNotImplemented
}
func (g *AsaasGateway) HandleWebhook(context.Context, []byte) (WebhookEvent, error) {
	return WebhookEvent{}, ErrNotImplemented
}
