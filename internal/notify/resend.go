package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ResendEndpoint é o endpoint de envio da API do Resend (documentação oficial). Não fixa
// domínio de envio — o from vem sempre de TIMBRE_MAIL_FROM.
const ResendEndpoint = "https://api.resend.com/emails"

// resendTimeout é o teto de uma chamada HTTP ao Resend. Constante isolada.
const resendTimeout = 10 * time.Second

// ResendProvider envia e-mail pela API do Resend (cliente HTTP direto, sem SDK). Chave de
// API nunca aparece em log.
type ResendProvider struct {
	apiKey  string
	from    string
	replyTo string
	client  *http.Client
}

// NewResendProvider constrói o provedor Resend.
func NewResendProvider(apiKey, from, replyTo string) *ResendProvider {
	return &ResendProvider{
		apiKey: apiKey, from: from, replyTo: replyTo,
		client: &http.Client{Timeout: resendTimeout},
	}
}

type resendAttachment struct {
	Filename    string `json:"filename"`
	Content     string `json:"content"`
	ContentType string `json:"content_type,omitempty"`
}

type resendRequest struct {
	From        string             `json:"from"`
	To          []string           `json:"to"`
	Subject     string             `json:"subject"`
	Text        string             `json:"text,omitempty"`
	HTML        string             `json:"html,omitempty"`
	ReplyTo     string             `json:"reply_to,omitempty"`
	Attachments []resendAttachment `json:"attachments,omitempty"`
}

type resendResponse struct {
	ID string `json:"id"`
}

// Send chama o endpoint do Resend e devolve o id da mensagem. 5xx/429 → erro retryable;
// 4xx → erro permanente.
func (p *ResendProvider) Send(ctx context.Context, m RenderedMessage) (string, error) {
	body := resendRequest{
		From: p.from, To: []string{m.To}, Subject: m.Subject,
		Text: m.Text, HTML: m.HTML, ReplyTo: p.replyTo,
	}
	if m.Attachment != nil {
		body.Attachments = []resendAttachment{{
			Filename: m.Attachment.Filename, Content: m.Attachment.Content, ContentType: m.Attachment.ContentType,
		}}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", permanentError("montar corpo: " + err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ResendEndpoint, bytes.NewReader(raw))
	if err != nil {
		return "", retryableError("montar request: " + err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", retryableError("http: " + err.Error())
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return "", retryableError(fmt.Sprintf("resend status %d: %s", resp.StatusCode, sanitize(string(data))))
		}
		return "", permanentError(fmt.Sprintf("resend status %d: %s", resp.StatusCode, sanitize(string(data))))
	}
	var out resendResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return "", retryableError("ler resposta: " + err.Error())
	}
	return out.ID, nil
}

// sanitize limita o corpo da resposta ao log (evita ruído/segredo acidental).
func sanitize(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
