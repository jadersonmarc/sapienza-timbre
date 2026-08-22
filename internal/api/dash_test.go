package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
)

// rawGet faz um GET e devolve status + corpo cru (para CSV).
func rawGet(t *testing.T, ts *httptest.Server, path, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestDashboardReflectsSale é o "pronto quando" da Etapa 1.7: uma venda aparece no
// painel do produtor (curva por lote, financeiro), e o check-in progride.
func TestDashboardReflectsSale(t *testing.T) {
	ts, pool, signer := setupSigned(t)
	_, owner := createProducer(t, ts, "Casa Painel", "owner@painel.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()

	eventID := createEvent(t, ts, owner, "Show Painel", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	_, _ = getEventLots(t, ts, owner, eventID)
	cbody := buyViaSession(t, ts, buyer(t, ts, pool, "c@x.com"), map[string]any{
		"event_id": eventID, "quantity": 2,
	}, "pix")
	asaasRef, _ := cbody["asaas_ref"].(string)
	confirmWebhook(t, ts, asaasRef)

	// Painel reflete a venda.
	code, body := do(t, ts, "GET", "/api/v1/dash/events/"+eventID, bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("dash: %d, %v", code, body)
	}
	sales, _ := body["sales"].([]any)
	if len(sales) != 1 {
		t.Fatalf("esperava 1 lote, veio %v", body["sales"])
	}
	lot0, _ := sales[0].(map[string]any)
	if lot0["sold_count"].(float64) != 2 || lot0["revenue_cents"].(float64) != 10000 {
		t.Fatalf("curva do lote: esperava sold_count 2 / revenue 10000, veio %v", lot0)
	}
	fin, _ := body["finance"].(map[string]any)
	if fin["gross_cents"].(float64) != 10000 || fin["taxa_cents"].(float64) != 900 || fin["repasse_cents"].(float64) != 10000 {
		t.Fatalf("financeiro: %v", fin)
	}
	chk, _ := body["checkin"].(map[string]any)
	if chk["tickets_total"].(float64) != 2 || chk["admitted"].(float64) != 0 {
		t.Fatalf("checkin inicial: %v", chk)
	}

	// Um check-in progride o painel.
	var token string
	var tid uuid.UUID
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT id FROM tickets LIMIT 1`).Scan(&tid); err != nil {
			t.Fatalf("ticket: %v", err)
		}
		var e error
		if token, e = ticketing.TicketToken(ctx, tx, tid); e != nil {
			t.Fatalf("token: %v", e)
		}
	})
	_ = signer
	if code, vb := do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner), map[string]any{"token": token, "gate": "G1"}); code != http.StatusOK || vb["verdict"] != "admitted" {
		t.Fatalf("validate: %d %v", code, vb)
	}
	_, body = do(t, ts, "GET", "/api/v1/dash/events/"+eventID, bearer(owner), nil)
	chk, _ = body["checkin"].(map[string]any)
	if chk["admitted"].(float64) != 1 {
		t.Fatalf("checkin após admissão: esperava 1, veio %v", chk)
	}

	// CSV exporta os ingressos.
	code, csvBody := rawGet(t, ts, "/api/v1/dash/events/"+eventID+"/export.csv", owner)
	if code != http.StatusOK || !strings.HasPrefix(csvBody, "ticket_id,lote,assento,status,emitido_em") {
		t.Fatalf("csv: %d, %q", code, csvBody)
	}
	if lines := strings.Count(strings.TrimSpace(csvBody), "\n"); lines != 2 { // header + 2 - 1
		t.Fatalf("csv esperava 2 ingressos, linhas=%d", lines)
	}
}

// TestAdminSummaryAndModeration cobre o painel administrativo.
func TestAdminSummaryAndModeration(t *testing.T) {
	ts, pool := setup(t)
	pidStr, owner := createProducer(t, ts, "Casa Admin", "owner@admin.com", "senha1234")
	pid := producerID(t, ts, owner)

	eventID := createEvent(t, ts, owner, "Show Admin", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil)
	_, _ = getEventLots(t, ts, owner, eventID)
	cbody := buyViaSession(t, ts, buyer(t, ts, pool, "buy@admin-dash.com"), map[string]any{
		"event_id": eventID, "quantity": 1,
	}, "pix")
	confirmWebhook(t, ts, cbody["asaas_ref"].(string))

	adminHdr := seedAdmin(t, ts, pool, "admin@dash.com", "super_admin")

	// Sem token → 401.
	if code, _ := do(t, ts, "GET", "/api/v1/admin/summary", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("admin sem token: esperava 401, veio %d", code)
	}

	code, body := do(t, ts, "GET", "/api/v1/admin/summary", adminHdr, nil)
	if code != http.StatusOK {
		t.Fatalf("admin summary: %d", code)
	}
	// Faturamento do dia = taxa de plataforma (modelo Sympla §4): 450 para uma venda de face 5000.
	if body["producers_total"].(float64) < 1 || body["revenue_today_cents"].(float64) < 400 {
		t.Fatalf("admin summary inesperado: %v", body)
	}

	// Suspende e reaprova o produtor.
	if code, _ := do(t, ts, "POST", "/api/v1/admin/producers/"+pidStr+"/suspend", adminHdr, nil); code != http.StatusOK {
		t.Fatalf("suspend: %d", code)
	}
	if st := scanProducerStatus(t, pool, pid); st != "suspended" {
		t.Fatalf("esperava suspended, veio %s", st)
	}
	if code, _ := do(t, ts, "POST", "/api/v1/admin/producers/"+pidStr+"/approve", adminHdr, nil); code != http.StatusOK {
		t.Fatalf("approve: %d", code)
	}
	if st := scanProducerStatus(t, pool, pid); st != "active" {
		t.Fatalf("esperava active, veio %s", st)
	}

	// Suspende o evento.
	if code, _ := do(t, ts, "POST", "/api/v1/admin/producers/"+pidStr+"/events/"+eventID+"/suspend", adminHdr, nil); code != http.StatusOK {
		t.Fatalf("suspend event: %d", code)
	}
}

func scanProducerStatus(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, pid uuid.UUID) string {
	t.Helper()
	var st string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM producers WHERE id=$1`, pid).Scan(&st); err != nil {
		t.Fatalf("status: %v", err)
	}
	return st
}
