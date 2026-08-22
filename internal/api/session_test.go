package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/checkout"
)

// createSession cria uma sessão anônima e devolve id + anon_token.
func createSession(t *testing.T, ts *httptest.Server, sel map[string]any) (string, string) {
	t.Helper()
	code, body := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil, sel)
	if code != http.StatusCreated {
		t.Fatalf("create session: %d %v", code, body)
	}
	id, _ := body["id"].(string)
	tok, _ := body["anon_token"].(string)
	if id == "" || tok == "" {
		t.Fatalf("sessão sem id/token: %v", body)
	}
	return id, tok
}

// bindAndPay faz bind+pay de uma sessão e devolve o corpo do pay.
func bindAndPay(t *testing.T, ts *httptest.Server, id string, buyerHdr map[string]string, method string) map[string]any {
	t.Helper()
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+id+"/bind", buyerHdr, nil); code != http.StatusOK {
		t.Fatalf("bind: %d", code)
	}
	code, body := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+id+"/pay", buyerHdr, map[string]any{"method": method})
	if code != http.StatusCreated {
		t.Fatalf("pay: %d %v", code, body)
	}
	return body
}

// TestSessionSelectionReservesExclusively: selecionar assentos sem conta cria a sessão e
// reserva; a reserva é exclusiva (segunda sessão nos mesmos assentos falha).
func TestSessionSelectionReservesExclusively(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Sess", "owner@sess.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, seats, _ := seatedEvent(t, ts, pool, owner, pid, 2)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	id, _ := createSession(t, ts, map[string]any{
		"event_id": eventID.String(), "quantity": 1, "seat_ids": []string{seats[0].String()},
	})
	// A reserva existe (seat_occupancy hold vivo).
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM seat_occupancy WHERE seat_id=$1 AND kind='hold' AND NOT released`, seats[0]); n != 1 {
		t.Fatalf("esperava 1 hold vivo, veio %d", n)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM checkout_sessions WHERE id=$1`, uuid.MustParse(id)); n != 1 {
		t.Fatalf("sessão não persistida")
	}
	// Segunda sessão no mesmo assento → 409 (exclusividade).
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil, map[string]any{
		"event_id": eventID.String(), "quantity": 1, "seat_ids": []string{seats[0].String()},
	}); code != http.StatusConflict {
		t.Fatalf("reserva exclusiva quebrada: esperava 409, veio %d", code)
	}
}

// TestPayOpenSessionRefused: pagar uma sessão 'open' (sem bind) é recusado.
func TestPayOpenSessionRefused(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Open", "owner@open.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Open", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	id, _ := createSession(t, ts, map[string]any{"event_id": eventID, "quantity": 1})
	_ = pid
	_ = ctx

	buyerHdr := buyer(t, ts, pool, "payopen@x.com")
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+id+"/pay", buyerHdr, map[string]any{"method": "pix"}); code != http.StatusConflict {
		t.Fatalf("pay em sessão open: esperava 409, veio %d", code)
	}
}

// TestBindOtherSubjectRefused: bind com subject diferente do que já vinculou é recusado.
func TestBindOtherSubjectRefused(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Bind", "owner@bind.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show Bind", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	id, _ := createSession(t, ts, map[string]any{"event_id": eventID, "quantity": 1})

	ana := buyer(t, ts, pool, "ana@bind.com")
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+id+"/bind", ana, nil); code != http.StatusOK {
		t.Fatalf("bind ana: %d", code)
	}
	bruno := buyer(t, ts, pool, "bruno@bind.com")
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+id+"/bind", bruno, nil); code != http.StatusConflict {
		t.Fatalf("bind de outro subject: esperava 409, veio %d", code)
	}
}

// TestBindExtendsReservationOnce: o bind estende a reserva UMA vez; a segunda tentativa não.
func TestBindExtendsReservationOnce(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Ext", "owner@ext.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, seats, _ := seatedEvent(t, ts, pool, owner, pid, 1)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	id, _ := createSession(t, ts, map[string]any{
		"event_id": eventID.String(), "quantity": 1, "seat_ids": []string{seats[0].String()},
	})
	readExp := func() time.Time {
		var t2 time.Time
		inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
			if err := tx.QueryRow(ctx, `SELECT expires_at FROM seat_occupancy WHERE seat_id=$1 AND kind='hold' AND NOT released`, seats[0]).Scan(&t2); err != nil {
				t.Fatalf("expires_at: %v", err)
			}
		})
		return t2
	}
	base := readExp()
	ana := buyer(t, ts, pool, "ana@ext.com")
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+id+"/bind", ana, nil); code != http.StatusOK {
		t.Fatalf("bind: %d", code)
	}
	after1 := readExp()
	if after1.Sub(base) < 30*time.Second {
		t.Fatalf("bind deveria estender a reserva (base=%v after=%v)", base, after1)
	}
	// Segundo bind (mesmo subject) NÃO estende de novo.
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+id+"/bind", ana, nil); code != http.StatusOK {
		t.Fatalf("bind 2: %d", code)
	}
	after2 := readExp()
	if !after2.Equal(after1) {
		t.Fatalf("segundo bind não deveria estender (after1=%v after2=%v)", after1, after2)
	}
}

// TestSessionExpiredDuringAccess: reserva expirada durante o acesso deixa a sessão
// 'expired' e libera os assentos.
func TestSessionExpiredDuringAccess(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Exp", "owner@exp2.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, seats, _ := seatedEvent(t, ts, pool, owner, pid, 1)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	id, _ := createSession(t, ts, map[string]any{
		"event_id": eventID.String(), "quantity": 1, "seat_ids": []string{seats[0].String()},
	})
	// Expira a sessão (reserva vencida durante o acesso).
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `UPDATE checkout_sessions SET expires_at=now()-interval '1 minute' WHERE id=$1`, uuid.MustParse(id)); err != nil {
			t.Fatalf("expirar: %v", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE seat_occupancy SET expires_at=now()-interval '1 minute' WHERE seat_id=$1 AND kind='hold'`, seats[0]); err != nil {
			t.Fatalf("expirar ocupação: %v", err)
		}
	})
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if _, err := checkout.ExpireOpenSessions(ctx, tx); err != nil {
			t.Fatalf("expire: %v", err)
		}
	})
	if st := scanStr(t, ctx, pool, pid, `SELECT status FROM checkout_sessions WHERE id=$1`, uuid.MustParse(id)); st != "expired" {
		t.Fatalf("esperava expired, veio %s", st)
	}
	// Assentos liberados: nova sessão no mesmo assento passa.
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil, map[string]any{
		"event_id": eventID.String(), "quantity": 1, "seat_ids": []string{seats[0].String()},
	}); code != http.StatusCreated {
		t.Fatalf("assento deveria estar liberado, veio %d", code)
	}
}

// TestResumeByAnonToken: retomar a sessão pelo anon_token devolve a seleção intacta.
func TestResumeByAnonToken(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducer(t, ts, "Casa Resume", "owner@resume.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show Resume", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	id, tok := createSession(t, ts, map[string]any{"event_id": eventID, "quantity": 2, "coupon_code": "TST"})

	// Sem token → 401; token errado → 401.
	if code, _ := do(t, ts, "GET", "/api/v1/public/checkout/sessions/"+id, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("sem anon_token: esperava 401, veio %d", code)
	}
	hdr := map[string]string{"X-Anon-Token": "errado"}
	if code, _ := do(t, ts, "GET", "/api/v1/public/checkout/sessions/"+id, hdr, nil); code != http.StatusUnauthorized {
		t.Fatalf("anon_token errado: esperava 401, veio %d", code)
	}

	hdr = map[string]string{"X-Anon-Token": tok}
	code, body := do(t, ts, "GET", "/api/v1/public/checkout/sessions/"+id, hdr, nil)
	if code != http.StatusOK {
		t.Fatalf("resume: %d", code)
	}
	items := body["items"].(map[string]any)
	if items["quantity"].(float64) != 2 || items["coupon_code"] != "TST" {
		t.Fatalf("seleção não retomada intacta: %v", items)
	}
}

// TestSessionCapPerIP: teto de sessões abertas por IP impede criação em massa.
func TestSessionCapPerIP(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducer(t, ts, "Casa Cap", "owner@cap.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show Cap", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	// Abre o teto (5) de sessões e verifica que a próxima é recusada.
	for i := 0; i < checkout.MaxOpenSessionsPerIP; i++ {
		code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil, map[string]any{"event_id": eventID, "quantity": 1})
		if code != http.StatusCreated {
			t.Fatalf("sessão %d: %d", i, code)
		}
	}
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil, map[string]any{"event_id": eventID, "quantity": 1}); code != http.StatusTooManyRequests {
		t.Fatalf("teto por IP: esperava 429, veio %d", code)
	}
}

// TestSweeperExpiresOpenReleasesReservation: a varredura expira sessão 'open' vencida e
// libera lote e assento.
func TestSweeperExpiresOpenReleasesReservation(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Sweep", "owner@sweep.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()

	// Pista: sessão reserva o lote (held_count++).
	eventID := createEvent(t, ts, owner, "Show Sweep", "shows")
	lotID := createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	createSession(t, ts, map[string]any{"event_id": eventID, "quantity": 2})
	if h := scanInt(t, ctx, pool, pid, `SELECT held_count FROM lots WHERE id=$1`, uuid.MustParse(lotID)); h != 2 {
		t.Fatalf("esperava held_count 2, veio %d", h)
	}
	// Expira todas as sessões abertas.
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `UPDATE checkout_sessions SET expires_at=now()-interval '1 minute' WHERE status='open'`); err != nil {
			t.Fatalf("expirar: %v", err)
		}
		if _, err := checkout.ExpireOpenSessions(ctx, tx); err != nil {
			t.Fatalf("expire: %v", err)
		}
	})
	if h := scanInt(t, ctx, pool, pid, `SELECT held_count FROM lots WHERE id=$1`, uuid.MustParse(lotID)); h != 0 {
		t.Fatalf("lote deveria ser liberado, veio %d", h)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM checkout_sessions WHERE status='expired'`); n == 0 {
		t.Fatalf("nenhuma sessão marcada expired")
	}
}

// TestGuestCheckoutRemoved: o caminho de convidado não existe mais (404).
func TestGuestCheckoutRemoved(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa NoGuest", "owner@noguest.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show NoGuest", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout", buyer(t, ts, pool, "x@x.com"), map[string]any{
		"event_id": eventID, "quantity": 1,
	}); code != http.StatusNotFound {
		t.Fatalf("convidado deveria ser 404, veio %d", code)
	}
}

// TestFullPurchaseWithNewAccount: conta criada no meio do fluxo chega ao QR.
func TestFullPurchaseWithNewAccount(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Full", "owner@full.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Full", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}

	// Seleção sem conta.
	id, _ := createSession(t, ts, map[string]any{"event_id": eventID, "quantity": 1})

	// Conta criada no meio do fluxo (OTP).
	ana := bearer(verifyOTP(t, ts, pool, "mid@x.com", "123456"))

	// Bind + pay.
	body := bindAndPay(t, ts, id, ana, "pix")
	confirmWebhook(t, ts, body["asaas_ref"].(string))

	// Ingresso ativo + QR assinado.
	tid := firstTicket(t, ctx, pool, pid)
	if st := scanStr(t, ctx, pool, pid, `SELECT status FROM tickets WHERE id=$1`, tid); st != "active" {
		t.Fatalf("ingresso deveria estar ativo, veio %s", st)
	}
	tok := tokenOf(t, ctx, pool, pid, tid)
	if tok == "" {
		t.Fatalf("QR vazio")
	}
}
