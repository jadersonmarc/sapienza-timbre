package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/jadersonmarc/sapienza-timbre/internal/config"
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
