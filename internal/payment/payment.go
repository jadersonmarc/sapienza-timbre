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
	MethodPix  = "pix"
	MethodCard = "credit_card"
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
	DueDate      time.Time
	Split        []SplitItem // divisão automática no ato da venda
}

// Charge é a cobrança criada.
type Charge struct {
	AsaasRef string
	Status   string
	PixCode  string // "copia e cola" do Pix (quando method = pix)
}

// WebhookEvent é o evento decodificado do webhook do gateway. A idempotência é do
// chamador (o checkout ignora um asaas_ref já confirmado).
type WebhookEvent struct {
	AsaasRef  string
	Type      string
	Confirmed bool // true quando o pagamento foi confirmado/recebido
}

// PaymentGateway é a interface com o gateway.
type PaymentGateway interface {
	CreateCharge(ctx context.Context, req ChargeRequest) (Charge, error)
	HandleWebhook(ctx context.Context, payload []byte) (WebhookEvent, error)
	Configured() bool
}
