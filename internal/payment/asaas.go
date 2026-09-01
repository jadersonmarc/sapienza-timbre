package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"
)

// AsaasGateway é o cliente HTTP real do Asaas (Pix e cartão). É usado quando
// ASAAS_API_KEY está setado; caso contrário o default é o FakeGateway. A cobrança nasce
// INTEIRA na conta da plataforma: não há recebedor secundário.
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

// CreateCharge cria o cliente e a cobrança no Asaas e busca o Pix.
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
	if req.ExternalReference != "" {
		payload["externalReference"] = req.ExternalReference
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

// refundRefusalMarkers classifica a recusa do gateway no estorno. Os três motivos levam a
// tratamentos diferentes — saldo insuficiente faz a plataforma cobrir a devolução, estorno
// repetido é sucesso disfarçado, e cobrança não estornável é erro de quem chamou.
//
// PROVISÓRIO: a classificação é por marcador no corpo da resposta, porque o Asaas devolve a
// causa em texto e o código de erro dele não é estável entre endpoints. Confirmar contra a
// documentação e trocar por código assim que houver um confiável.
var refundRefusalMarkers = []struct {
	marker string
	err    error
}{
	// MEDIDO no sandbox: a recusa por estorno concorrente vem como invalid_object com esta
	// frase. Ela é ESPERA, não falha — e sem o marcador caía em erro desconhecido, que o
	// caminho manual do produtor mostrava como "não deu".
	{"estorno dessa cobrança já está em andamento", ErrRefundInProgress},
	{"refund is already in progress", ErrRefundInProgress},
	{"saldo insuficiente", ErrRefundInsufficientFunds},
	{"insufficient", ErrRefundInsufficientFunds},
	{"já estornad", ErrRefundAlreadyExists},
	{"already refunded", ErrRefundAlreadyExists},
	{"não pode ser estornad", ErrRefundNotRefundable},
	{"cannot be refunded", ErrRefundNotRefundable},
	{"not refundable", ErrRefundNotRefundable},
}

// classifyRefundErr traduz a recusa do gateway para um dos erros que o chamador distingue,
// preservando a mensagem original no wrap. Sem marcador conhecido, devolve o erro cru: um
// motivo que não sabemos ler não pode virar "saldo insuficiente" por descarte.
func classifyRefundErr(err error) error {
	low := strings.ToLower(err.Error())
	for _, m := range refundRefusalMarkers {
		if strings.Contains(low, m.marker) {
			return fmt.Errorf("%w: %s", m.err, err.Error())
		}
	}
	// Motivo desconhecido: é ele que precisa virar marcador. Sem esta linha, a primeira
	// recusa de um tipo novo some num 500 genérico e o código continua adivinhando.
	slog.Warn("asaas: recusa de estorno NÃO classificada — confirmar o marcador", "motivo", err.Error())
	return err
}

// Refund devolve o valor da cobrança ao comprador. Sem ValueCents, o Asaas estorna o
// integral; com ele, estorna parcial — e uma cobrança aceita vários parciais, por isso a
// idempotência não pode ser pela cobrança (ver RefundRequest.Description).
func (g *AsaasGateway) Refund(ctx context.Context, req RefundRequest) (Refund, error) {
	if req.AsaasRef == "" {
		return Refund{}, fmt.Errorf("cobrança obrigatória para estornar")
	}
	payload := map[string]any{}
	if req.ValueCents > 0 {
		payload["value"] = float64(req.ValueCents) / 100
	}
	if req.Description != "" {
		payload["description"] = req.Description
	}
	// A resposta é o objeto da COBRANÇA, não do estorno: o `id` e o `value` do topo são os
	// dela. A devolução recém-criada é a entrada de `refunds[]` com a nossa description —
	// ler o topo daria o id da cobrança e o valor total, que foi o que este código fez até
	// medirmos contra o sandbox.
	var out struct {
		Refunds []struct {
			Description string  `json:"description"`
			Status      string  `json:"status"`
			Value       float64 `json:"value"`
		} `json:"refunds"`
	}
	raw, err := g.rawPost(ctx, "/v3/payments/"+req.AsaasRef+"/refund", payload)
	if err != nil {
		return Refund{}, classifyRefundErr(fmt.Errorf("estornar cobrança asaas: %w", err))
	}
	// A FORMA da resposta (só os caminhos de chave, sem os valores) fica no log da primeira
	// devolução real. É ela que responde se o gateway devolve id de estorno próprio e se há
	// campo de idempotência — perguntas que hoje são remendo no código por falta de um
	// estorno de verdade para observar.
	slog.Info("asaas: forma da resposta de estorno", "shape", ShapeOf(raw))
	if err := json.Unmarshal(raw, &out); err != nil {
		return Refund{}, fmt.Errorf("estorno asaas: resposta ilegível: %w", err)
	}
	r := Refund{Description: req.Description, ValueCents: req.ValueCents}
	for _, ref := range out.Refunds {
		if ref.Description == req.Description {
			r.Status = ref.Status
			if v := int64(math.Round(ref.Value * 100)); v > 0 {
				r.ValueCents = v
			}
			break
		}
	}
	return r, nil
}

// asaasWebhook é o formato do webhook do Asaas.
type asaasWebhook struct {
	// ID do EVENTO — é por ele que a idempotência funciona.
	ID      string `json:"id"`
	Event   string `json:"event"`
	Payment struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		// O objeto da cobrança embutido no aviso carrega as devoluções. Se elas vierem, a
		// description de cada uma é a NOSSA chave de volta — e é ela que distingue o eco do
		// nosso próprio estorno de uma devolução feita por fora.
		Refunds []struct {
			Description string  `json:"description"`
			Status      string  `json:"status"`
			Value       float64 `json:"value"`
		} `json:"refunds"`
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
	var refundKeys []string
	for _, r := range e.Payment.Refunds {
		if r.Description != "" {
			refundKeys = append(refundKeys, r.Description)
		}
	}
	return WebhookEvent{
		ID: e.ID, AsaasRef: e.Payment.ID, Type: e.Event, RefundKeys: refundKeys,
		Confirmed: confirmed, Refunded: refunded,
	}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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

// Fees busca a tabela de tarifas vigente da conta. Guarda a resposta crua junto: é ela que
// vai para o snapshot de auditoria da venda.
func (g *AsaasGateway) Fees(ctx context.Context) (Fees, error) {
	raw, err := g.raw(ctx, http.MethodGet, "/v3/myAccount/fees")
	if err != nil {
		return Fees{}, err
	}
	return parseAsaasFees(raw)
}

// raw faz a chamada e devolve o corpo sem decodificar (para guardar o original).
func (g *AsaasGateway) raw(ctx context.Context, method, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, g.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("access_token", g.apiKey)
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("asaas %s %s: status %d: %s", method, path, resp.StatusCode, clip(string(body)))
	}
	return body, nil
}

// rawPost faz um POST e devolve o corpo CRU, para quem precisa da resposta inteira e não só
// dos campos que sabe ler.
func (g *AsaasGateway) rawPost(ctx context.Context, path string, body any) ([]byte, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+path, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("access_token", g.apiKey)
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("asaas POST %s: status %d: %s", path, resp.StatusCode, clip(string(out)))
	}
	return out, nil
}

// RawGet devolve o corpo cru de um GET. Existe para a sonda do sandbox inspecionar a FORMA
// de uma resposta sem que o cliente precise saber ler todos os campos dela.
func (g *AsaasGateway) RawGet(ctx context.Context, path string) ([]byte, error) {
	return g.raw(ctx, http.MethodGet, path)
}

// clip limita o corpo de erro que entra em log/mensagem: o suficiente para diagnosticar,
// sem despejar resposta inteira.
func clip(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}
