package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		baseURL: normalizeBase(baseURL),
		http:    &http.Client{Timeout: 20 * time.Second},
	}
}

// normalizeBase tolera a base copiada da documentação com a versão junto. Todo path daqui
// já começa em "/v3", então uma base terminada em "/v3" produziria "/v3/v3/..." — que
// autentica e devolve 404, um erro que não se parece nada com o que é. Um "/api" final é
// preservado: é o formato do host legado (…/api/v3).
func normalizeBase(baseURL string) string {
	b := strings.TrimRight(baseURL, "/")
	return strings.TrimSuffix(b, "/v3")
}

// BaseURL é a base efetiva usada nas chamadas (registrada no boot para diagnóstico).
func (g *AsaasGateway) BaseURL() string { return g.baseURL }

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
		// O corpo é onde o Asaas diz a causa ("carteira inválida", "cpfCnpj obrigatório"…).
		// Sem ele sobra um número, e o 500 vira adivinhação. Limitado para não despejar
		// resposta inteira em log.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("asaas %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(detail)))
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
	customer := map[string]any{
		"name": nonEmpty(req.BuyerName, "Comprador"), "email": req.BuyerEmail, "cpfCnpj": req.BuyerCPF,
	}
	if req.BuyerPhone != "" {
		customer["mobilePhone"] = req.BuyerPhone
	}
	if err := g.do(ctx, http.MethodPost, "/v3/customers", customer, &cust); err != nil {
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
		// Manda a contagem e o TOTAL: dividir aqui perderia centavos na divisão inteira
		// (3× de 100,00 viraria 99,99) e o gateway sabe distribuir a sobra.
		payload["installmentCount"] = req.Installments
		payload["totalValue"] = reais(req.AmountCents)
	}
	// Cartão transparente: os dados vão nesta chamada e o gateway captura na hora. Cartão
	// recusado falha aqui, com o motivo vindo do gateway.
	if req.Method == MethodCard && req.Card != nil {
		payload["creditCard"] = map[string]any{
			"holderName":  req.Card.HolderName,
			"number":      req.Card.Number,
			"expiryMonth": req.Card.ExpiryMonth,
			"expiryYear":  req.Card.ExpiryYear,
			"ccv":         req.Card.CCV,
		}
		if req.Holder != nil {
			payload["creditCardHolderInfo"] = map[string]any{
				"name":          req.Holder.Name,
				"email":         req.Holder.Email,
				"cpfCnpj":       req.Holder.TaxID,
				"postalCode":    req.Holder.PostalCode,
				"addressNumber": req.Holder.AddressNumber,
				"phone":         req.Holder.Phone,
			}
		}
		if req.RemoteIP != "" {
			payload["remoteIp"] = req.RemoteIP
		}
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
		ID         string `json:"id"`
		Status     string `json:"status"`
		InvoiceURL string `json:"invoiceUrl"`
	}
	if err := g.do(ctx, http.MethodPost, "/v3/payments", payload, &pay); err != nil {
		return Charge{}, fmt.Errorf("criar cobrança asaas: %w", err)
	}

	c := Charge{AsaasRef: pay.ID, Status: pay.Status, InvoiceURL: pay.InvoiceURL}
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
	refunded := e.Event == "PAYMENT_REFUNDED" || e.Event == "PAYMENT_CHARGEBACK_REQUESTED"
	return WebhookEvent{AsaasRef: e.Payment.ID, Type: e.Event, Confirmed: confirmed, Refunded: refunded}, nil
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

// CreateAccount abre a subconta do produtor no Asaas (o serviço hoje se chama BaaS —
// antigo "white label"; o endpoint segue /v3/accounts). Devolve só o walletId:
// é o destinatário do split. A resposta traz também a chave de API da subconta — ela NÃO é
// guardada, porque operar em nome do produtor não é necessário para dividir a venda, e
// custodiar credencial de terceiro é responsabilidade que não vale assumir de graça.
func (g *AsaasGateway) CreateAccount(ctx context.Context, in AccountInput) (Account, error) {
	payload := map[string]any{
		"name":          in.Name,
		"email":         in.Email,
		"cpfCnpj":       in.TaxID,
		"mobilePhone":   in.MobilePhone,
		"incomeValue":   reais(in.IncomeCents),
		"address":       in.Address,
		"addressNumber": in.AddressNumber,
		"province":      in.Province,
		"postalCode":    in.PostalCode,
	}
	if in.CompanyType != "" {
		payload["companyType"] = in.CompanyType
	} else if in.BirthDate != "" {
		payload["birthDate"] = in.BirthDate
	}
	var out struct {
		WalletID string `json:"walletId"`
		ID       string `json:"id"`
	}
	if err := g.do(ctx, http.MethodPost, "/v3/accounts", payload, &out); err != nil {
		return Account{}, fmt.Errorf("criar conta de recebimento: %w", err)
	}
	if out.WalletID == "" {
		return Account{}, fmt.Errorf("gateway não devolveu a carteira da conta criada")
	}
	return Account{WalletID: out.WalletID}, nil
}
