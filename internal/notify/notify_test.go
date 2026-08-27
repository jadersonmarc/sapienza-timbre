package notify

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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

	enqueue(t, ctx, pool, svc, Message{Kind: KindAuthCode, To: "retry@x.com", Code: "111111", CodeMinutes: 10})
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
	enqueue(t, ctx, pool, svc, Message{Kind: KindTicket, To: "ok@x.com", EventName: "Show"})
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
	enqueue(t, context.Background(), pool, svc, Message{Kind: KindAuthCode, To: "nao@bloqueia.com", Code: "1", CodeMinutes: 5})
}

// enqueue grava a mensagem na outbox pela transação do chamador, que é como o Send é usado
// de verdade: a mensagem nasce dentro da transação de quem a produziu.
func enqueue(t *testing.T, ctx context.Context, pool *pgxpool.Pool, svc *Service, m Message) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := svc.Send(ctx, tx, m); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestRollbackLevaAMensagemJunto é o defeito de atomicidade que a outbox resolve: a
// mensagem era gravada pelo pool, fora da transação da venda, e um rollback tardio mandava
// o ingresso de uma compra que não existe.
func TestRollbackLevaAMensagemJunto(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	svc := NewService(pool, "https://timbre.sapienzalabs.com.br")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := svc.Send(ctx, tx, Message{Kind: KindTicket, To: "fantasma@x.com", EventName: "Venda que falhou"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// A venda falha depois de a mensagem ter sido enfileirada.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE to_email='fantasma@x.com'`).Scan(&n); err != nil {
		t.Fatalf("contar: %v", err)
	}
	if n != 0 {
		t.Fatalf("a mensagem deveria ter ido embora com o rollback, veio %d", n)
	}
}

// TestDoisWorkersNaoEntregamDuasVezes é o defeito de lock: a seleção era pelo pool, o lock
// soltava na hora, e duas réplicas entregavam a mesma mensagem. Agora o lock vive o tempo
// do processamento.
func TestDoisWorkersNaoEntregamDuasVezes(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	svc := NewService(pool, "https://timbre.sapienzalabs.com.br")
	for i := range 8 {
		enqueue(t, ctx, pool, svc, Message{
			Kind: KindTicket, To: fmt.Sprintf("dup%d@x.com", i), EventName: "Show",
			IdempotencyKey: fmt.Sprintf("t:%d", i),
		})
	}

	var mu sync.Mutex
	entregas := map[string]int{}
	conta := fakeProvider{fn: func(m RenderedMessage) (string, error) {
		mu.Lock()
		entregas[m.To]++
		mu.Unlock()
		// Segura um instante: sem o lock dentro da transação, é nesta janela que a segunda
		// réplica pega a mesma linha.
		time.Sleep(20 * time.Millisecond)
		return "prov-1", nil
	}}

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = NewWorker(pool, conta).Process(ctx)
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for to, n := range entregas {
		if n != 1 {
			t.Fatalf("%s recebeu %d vezes — duas réplicas entregaram a mesma mensagem", to, n)
		}
	}
	if len(entregas) != 8 {
		t.Fatalf("esperava 8 entregas distintas, veio %d", len(entregas))
	}
}

// TestSucessoNaoReenviaOQueJaSaiu: uma passada posterior não pode reentregar mensagem já
// entregue — é o que separa "ao menos uma vez" de "toda vez".
func TestSucessoNaoReenviaOQueJaSaiu(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	svc := NewService(pool, "https://timbre.sapienzalabs.com.br")
	enqueue(t, ctx, pool, svc, Message{Kind: KindTicket, To: "unico@x.com", EventName: "Show"})

	var envios int
	conta := fakeProvider{fn: func(RenderedMessage) (string, error) {
		envios++
		return "prov-1", nil
	}}
	w := NewWorker(pool, conta)
	for range 3 {
		if err := w.Process(ctx); err != nil {
			t.Fatalf("process: %v", err)
		}
	}
	if envios != 1 {
		t.Fatalf("esperava 1 envio, veio %d", envios)
	}
}

// TestChaveDeIdempotenciaNaoDuplicaNaEntrada: o mesmo aviso enfileirado duas vezes (webhook
// reprocessado) entra uma vez só.
func TestChaveDeIdempotenciaNaoDuplicaNaEntrada(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	svc := NewService(pool, "https://timbre.sapienzalabs.com.br")
	m := Message{Kind: KindTicket, To: "repetido@x.com", EventName: "Show", IdempotencyKey: "ticket:abc"}
	enqueue(t, ctx, pool, svc, m)
	enqueue(t, ctx, pool, svc, m)

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE to_email='repetido@x.com'`).Scan(&n); err != nil {
		t.Fatalf("contar: %v", err)
	}
	if n != 1 {
		t.Fatalf("esperava 1 mensagem, veio %d", n)
	}
}
