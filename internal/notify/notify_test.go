package notify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jadersonmarc/sapienza-timbre/internal/testutil"
)

type fakeProvider struct {
	fn func(RenderedMessage) (string, error)
}

func (f fakeProvider) Send(_ context.Context, m RenderedMessage) (string, error) {
	return f.fn(m)
}

// TestRenderAuthCodeSubject: o assunto contém o código (lido na notificação do celular);
// o corpo tem validade e a linha de "ignore"; sem link direto (perderia a seleção).
func TestRenderAuthCodeSubject(t *testing.T) {
	m := Message{Kind: KindAuthCode, To: "a@x.com", Code: "123456", CodeMinutes: 10}
	r := render(m, "https://timbre.sapienzalabs.com.br")
	if r.Subject != "Seu código: 123456" {
		t.Fatalf("assunto deveria conter o código: %s", r.Subject)
	}
	if !strings.Contains(r.Text, "10 minutos") || !strings.Contains(r.Text, "ignorar") {
		t.Fatalf("corpo deveria ter validade e aviso: %s", r.Text)
	}
	if strings.Contains(r.HTML, "http") {
		t.Fatalf("código de acesso não deveria ter link: %s", r.HTML)
	}
}

// TestRenderTicketAttachment: o ingresso sai com o QR como anexo de imagem E o link de
// meus ingressos.
func TestRenderTicketAttachment(t *testing.T) {
	m := Message{Kind: KindTicket, To: "a@x.com", EventName: "Show X", QRContent: "tok"}
	r := render(m, "https://timbre.sapienzalabs.com.br")
	if r.Attachment == nil || r.Attachment.ContentType != "image/png" || r.Attachment.Filename == "" {
		t.Fatalf("esperava anexo PNG do QR: %+v", r.Attachment)
	}
	if !strings.Contains(r.Text, "/ingressos") {
		t.Fatalf("corpo deveria ter o link de meus ingressos: %s", r.Text)
	}
	if r.Subject != "Show X" {
		t.Fatalf("assunto deveria ser o nome do evento: %s", r.Subject)
	}
}

// TestWorkerRetryAndProviderID: retry acontece em erro retryable até maxAttempts (depois
// failed); sucesso grava status sent + id do provedor.
func TestWorkerRetryAndProviderID(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	svc := NewService(pool, "https://timbre.sapienzalabs.com.br")

	if err := svc.Send(ctx, Message{Kind: KindAuthCode, To: "retry@x.com", Code: "111111", CodeMinutes: 10}); err != nil {
		t.Fatalf("send: %v", err)
	}
	fail := fakeProvider{fn: func(RenderedMessage) (string, error) {
		return "", retryableError("provedor fora do ar")
	}}
	w := NewWorker(pool, fail)
	w.MaxAttempts = 3
	w.Backoff = func(int) time.Duration { return 0 }
	for i := 0; i < 3; i++ {
		if err := w.Process(ctx); err != nil {
			t.Fatalf("process: %v", err)
		}
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM public.notifications WHERE to_email='retry@x.com'`).Scan(&status); err != nil {
		t.Fatalf("ler: %v", err)
	}
	if status != "failed" {
		t.Fatalf("esperava failed após esgotar tentativas, veio %s", status)
	}

	// Sucesso: sent + id do provedor.
	if err := svc.Send(ctx, Message{Kind: KindTicket, To: "ok@x.com", EventName: "Show"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	ok := fakeProvider{fn: func(RenderedMessage) (string, error) { return "re_ok", nil }}
	w2 := NewWorker(pool, ok)
	w2.Backoff = func(int) time.Duration { return 0 }
	if err := w2.Process(ctx); err != nil {
		t.Fatalf("process: %v", err)
	}
	var pid, st string
	if err := pool.QueryRow(ctx, `SELECT provider_message_id, status FROM public.notifications WHERE to_email='ok@x.com'`).Scan(&pid, &st); err != nil {
		t.Fatalf("ler: %v", err)
	}
	if st != "sent" || pid != "re_ok" {
		t.Fatalf("esperava sent + re_ok, veio %s/%s", st, pid)
	}
}

// TestProviderErrorDoesNotBlockSend: Service.Send enfileira e retorna nil mesmo se o
// provedor estiver fora — o envio é assíncrono.
func TestProviderErrorDoesNotBlockSend(t *testing.T) {
	pool := testutil.Pool(t)
	svc := NewService(pool, "https://timbre.sapienzalabs.com.br")
	if err := svc.Send(context.Background(), Message{Kind: KindAuthCode, To: "nao@bloqueia.com", Code: "1", CodeMinutes: 5}); err != nil {
		t.Fatalf("Send não deveria bloquear com provedor fora: %v", err)
	}
}
