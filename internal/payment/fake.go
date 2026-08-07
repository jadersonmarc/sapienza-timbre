package payment

import (
	"context"
	"encoding/json"
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
