package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// redirectTo faz o cliente do provedor falar com o servidor de teste no lugar da API real
// (o endpoint do Resend é constante de produção — não se troca por env).
type redirectTo struct{ base *url.URL }

func (r redirectTo) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme, req.URL.Host = r.base.Scheme, r.base.Host
	return http.DefaultTransport.RoundTrip(req)
}

func resendAgainst(t *testing.T, h http.HandlerFunc) *ResendProvider {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	p := NewResendProvider("re_secreta", "Timbre <nao-responda@exemplo.com>", "contato@exemplo.com")
	p.client = &http.Client{Transport: redirectTo{base: base}}
	return p
}

// TestResendRequestShape: o que sai para o Resend é o e-mail do ingresso completo —
// remetente configurado, destinatário, assunto, texto+html e o QR como anexo — com a
// chave no header (nunca no corpo).
func TestResendRequestShape(t *testing.T) {
	var got resendRequest
	var authz string
	p := resendAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		authz = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"re_123"}`))
	})

	msg := render(Message{
		Kind: KindTicket, To: "quem@comprou.com", EventName: "Show Rs",
		EventStarts: "01/09/2026 21:00", VenueCity: "Belo Horizonte",
		QRContent: "payload.assinatura",
	}, "https://timbre.sapienzalabs.com.br")

	id, err := p.Send(context.Background(), msg)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if id != "re_123" {
		t.Fatalf("esperava o id do provedor, veio %q", id)
	}
	if authz != "Bearer re_secreta" {
		t.Fatalf("chave deveria ir no header, veio %q", authz)
	}
	if got.From != "Timbre <nao-responda@exemplo.com>" || len(got.To) != 1 || got.To[0] != "quem@comprou.com" {
		t.Fatalf("remetente/destinatário errados: %+v", got)
	}
	if got.Subject != "Show Rs" || got.Text == "" || got.HTML == "" {
		t.Fatalf("assunto/corpo errados: %+v", got)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].Filename != "ingresso.png" || got.Attachments[0].Content == "" {
		t.Fatalf("o QR deveria ir anexado: %+v", got.Attachments)
	}
}

// TestResendErrorClassification: 5xx e 429 voltam para a fila; 4xx morre na primeira
// (repetir um "domínio não verificado" não conserta nada).
func TestResendErrorClassification(t *testing.T) {
	cases := []struct {
		status    int
		retryable bool
	}{
		{http.StatusInternalServerError, true},
		{http.StatusTooManyRequests, true},
		{http.StatusUnprocessableEntity, false},
		{http.StatusUnauthorized, false},
	}
	for _, c := range cases {
		p := resendAgainst(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(`{"message":"erro"}`))
		})
		_, err := p.Send(context.Background(), RenderedMessage{To: "x@y.com", Subject: "s"})
		if err == nil {
			t.Fatalf("status %d deveria falhar", c.status)
		}
		if isRetryable(err) != c.retryable {
			t.Fatalf("status %d: retryable=%v, esperava %v (%v)", c.status, isRetryable(err), c.retryable, err)
		}
	}
}
