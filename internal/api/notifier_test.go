package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jadersonmarc/sapienza-timbre/internal/config"
	"github.com/jadersonmarc/sapienza-timbre/internal/notify"
)

// TestNotifierConfigValidation: resend sem chave falha; log passa; valor desconhecido falha.
func TestNotifierConfigValidation(t *testing.T) {
	t.Setenv("DATABASE_URL", "x")
	t.Setenv("TIMBRE_JWT_SECRET", "y")

	t.Setenv("TIMBRE_NOTIFIER", "resend")
	t.Setenv("TIMBRE_RESEND_API_KEY", "")
	if _, err := config.Load(); err == nil {
		t.Fatalf("resend sem chave deveria falhar na inicialização")
	}

	t.Setenv("TIMBRE_NOTIFIER", "log")
	if _, err := config.Load(); err != nil {
		t.Fatalf("log deveria manter o comportamento atual: %v", err)
	}

	t.Setenv("TIMBRE_NOTIFIER", "pombo-correio")
	if _, err := config.Load(); err == nil {
		t.Fatalf("valor desconhecido deveria falhar (nada de default silencioso)")
	}
}

// TestTicketNotificationsCount: compra de quatro entradas gera quatro mensagens de ingresso.
func TestTicketNotificationsCount(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Nt", "owner@nt.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Nt", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	body := buyViaSession(t, ts, buyer(t, ts, pool, "quatro@nt.com"), map[string]any{"event_id": eventID, "quantity": 4}, "pix")
	confirmWebhook(t, ts, body["asaas_ref"].(string))

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM public.notifications WHERE kind='ticket_issued' AND to_email='quatro@nt.com'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 4 {
		t.Fatalf("esperava 4 mensagens de ingresso, veio %d", n)
	}
	if tc := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE order_id IS NOT NULL`); tc != 4 {
		t.Fatalf("esperava 4 ingressos, veio %d", tc)
	}
}

// TestAuthCodeNotificationRecorded: o pedido de código registra a notificação.
func TestAuthCodeNotificationRecorded(t *testing.T) {
	ts, pool := setup(t)
	if code, _ := do(t, ts, "POST", "/api/v1/public/auth/request-code", nil, map[string]any{"email": "codigo@nt.com"}); code != http.StatusOK {
		t.Fatalf("request-code: %d", code)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM public.notifications WHERE kind='auth_code' AND to_email='codigo@nt.com'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("esperava 1 notificação de código, veio %d", n)
	}
}

// TestResendCreatesNewRecord: reenvio pelo painel cria novo registro, sem duplicar ingresso.
func TestResendCreatesNewRecord(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Rs", "owner@rs.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Rs", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	body := buyViaSession(t, ts, buyer(t, ts, pool, "reenvio@rs.com"), map[string]any{"event_id": eventID, "quantity": 1}, "pix")
	confirmWebhook(t, ts, body["asaas_ref"].(string))

	// Pega o id da notificação (deste evento) e reenvia.
	var notifID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM public.notifications WHERE kind='ticket_issued' AND event_id=$1 LIMIT 1`, uuid.MustParse(eventID)).Scan(&notifID); err != nil {
		t.Fatalf("ler notificação: %v", err)
	}
	if code, _ := do(t, ts, "POST", "/api/v1/dash/notifications/"+notifID+"/resend", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("resend: %d", code)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM public.notifications WHERE kind='ticket_issued' AND ticket_id IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("reenvio deveria criar novo registro (2), veio %d", n)
	}
	if tc := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets`); tc != 1 {
		t.Fatalf("reenvio não deveria duplicar ingresso, veio %d", tc)
	}
}

// TestPurchaseEmailDelivered fecha o caminho inteiro do e-mail de compra: pagamento
// confirmado → mensagem enfileirada → worker drena → provedor recebe o e-mail com o QR
// anexado → registro vira 'sent' com o id do provedor. É a prova de que o disparo
// acontece, não só de que a fila enche.
func TestPurchaseEmailDelivered(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Dl", "owner@dl.com", "senha1234")
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Dl", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	body := buyViaSession(t, ts, buyer(t, ts, pool, "entrega@dl.com"), map[string]any{"event_id": eventID, "quantity": 1}, "pix")
	confirmWebhook(t, ts, body["asaas_ref"].(string))

	var got []notify.RenderedMessage
	provider := captureProvider{fn: func(m notify.RenderedMessage) (string, error) {
		got = append(got, m)
		return "re_entrega", nil
	}}
	w := notify.NewWorker(pool, provider)
	w.Backoff = func(int) time.Duration { return 0 }
	if err := w.Process(ctx); err != nil {
		t.Fatalf("worker: %v", err)
	}

	var ticketMsg *notify.RenderedMessage
	for i := range got {
		if got[i].To == "entrega@dl.com" && got[i].Attachment != nil {
			ticketMsg = &got[i]
		}
	}
	if ticketMsg == nil {
		t.Fatalf("o comprador não recebeu o e-mail do ingresso (mensagens entregues: %d)", len(got))
	}
	if ticketMsg.Subject != "Show Dl" {
		t.Fatalf("assunto deveria ser o nome do evento, veio %q", ticketMsg.Subject)
	}
	if ticketMsg.Attachment.Filename != "ingresso.png" || ticketMsg.Attachment.Content == "" {
		t.Fatalf("o QR deveria vir anexado, veio %+v", ticketMsg.Attachment)
	}
	if !strings.Contains(ticketMsg.Text, "/ingressos") {
		t.Fatalf("o corpo deveria linkar meus ingressos, veio %q", ticketMsg.Text)
	}

	var status, providerID string
	if err := pool.QueryRow(ctx, `
		SELECT status, provider_message_id FROM public.notifications
		 WHERE kind='ticket_issued' AND to_email='entrega@dl.com'`).Scan(&status, &providerID); err != nil {
		t.Fatalf("ler notificação: %v", err)
	}
	if status != "sent" || providerID != "re_entrega" {
		t.Fatalf("esperava sent/re_entrega, veio %s/%s", status, providerID)
	}
}

// captureProvider registra o e-mail pronto que iria ao provedor real.
type captureProvider struct {
	fn func(notify.RenderedMessage) (string, error)
}

func (c captureProvider) Send(_ context.Context, m notify.RenderedMessage) (string, error) {
	return c.fn(m)
}
