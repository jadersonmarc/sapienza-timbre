package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// AsaasGateway é o cliente HTTP real do Asaas (Pix e cartão, com split no ato). É
// usado quando ASAAS_API_KEY está setado; caso contrário o default é o FakeGateway.
// Best-effort: validar contra o sandbox do Asaas antes de produção.
type AsaasGateway struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

// NewAsaas constrói o gateway. baseURL vazio usa produção; para sandbox passe
// https://api-sandbox.asaas.com.
func NewAsaas(apiKey, baseURL string) *AsaasGateway {
	if baseURL == "" {
		baseURL = "https://api.asaas.com"
	}
	return &AsaasGateway{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 20 * time.Second},
	}
}

// Configured diz se há credencial para operar.
func (g *AsaasGateway) Configured() bool { return g.apiKey != "" }

func (g *AsaasGateway) do(ctx context.Context, method, path string, body any, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, g.baseURL+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("access_token", g.apiKey)
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("asaas %s %s: status %d", method, path, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// CreateCharge cria o cliente e a cobrança no Asaas, com split, e busca o Pix.
func (g *AsaasGateway) CreateCharge(ctx context.Context, req ChargeRequest) (Charge, error) {
	// 1. Cliente (idempotente por CPF seria melhor; aqui cria sempre).
	var cust struct {
		ID string `json:"id"`
	}
	if err := g.do(ctx, http.MethodPost, "/v3/customers", map[string]any{
		"name": nonEmpty(req.BuyerName, "Comprador"), "email": req.BuyerEmail, "cpfCnpj": req.BuyerCPF,
	}, &cust); err != nil {
		return Charge{}, fmt.Errorf("criar cliente asaas: %w", err)
	}

	billing := "PIX"
	if req.Method == MethodCard {
		billing = "CREDIT_CARD"
	}
	payload := map[string]any{
		"customer":    cust.ID,
		"billingType": billing,
		"value":       reais(req.AmountCents),
		"dueDate":     dueDate(req.DueDate),
	}
	if req.Method == MethodCard && req.Installments > 1 {
		payload["installmentCount"] = req.Installments
		payload["installmentValue"] = reais(req.AmountCents / int64(req.Installments))
	}
	if len(req.Split) > 0 {
		split := make([]map[string]any, 0, len(req.Split))
		for _, s := range req.Split {
			item := map[string]any{"walletId": s.WalletID}
			if s.FixedCents > 0 {
				item["fixedValue"] = reais(s.FixedCents)
			} else if s.Percent > 0 {
				item["percentualValue"] = s.Percent
			}
			split = append(split, item)
		}
		payload["split"] = split
	}

	var pay struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := g.do(ctx, http.MethodPost, "/v3/payments", payload, &pay); err != nil {
		return Charge{}, fmt.Errorf("criar cobrança asaas: %w", err)
	}

	c := Charge{AsaasRef: pay.ID, Status: pay.Status}
	if req.Method == MethodPix {
		var qr struct {
			Payload string `json:"payload"`
		}
		if err := g.do(ctx, http.MethodGet, "/v3/payments/"+pay.ID+"/pixQrCode", nil, &qr); err == nil {
			c.PixCode = qr.Payload
		}
	}
	return c, nil
}

// asaasWebhook é o formato do webhook do Asaas.
type asaasWebhook struct {
	Event   string `json:"event"`
	Payment struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"payment"`
}

// HandleWebhook decodifica e normaliza o webhook do Asaas.
func (g *AsaasGateway) HandleWebhook(_ context.Context, payload []byte) (WebhookEvent, error) {
	var e asaasWebhook
	if err := json.Unmarshal(payload, &e); err != nil {
		return WebhookEvent{}, err
	}
	confirmed := e.Event == "PAYMENT_CONFIRMED" || e.Event == "PAYMENT_RECEIVED"
	return WebhookEvent{AsaasRef: e.Payment.ID, Type: e.Event, Confirmed: confirmed}, nil
}

func reais(cents int64) string { return fmt.Sprintf("%.2f", float64(cents)/100.0) }

func dueDate(t time.Time) string {
	if t.IsZero() {
		t = time.Now().Add(24 * time.Hour)
	}
	return t.Format("2006-01-02")
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
