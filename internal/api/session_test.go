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
	return createSessionWithToken(t, ts, sel, "")
}

// createSessionWithToken cria uma sessão com um anon_token fornecido (retomada/próprio).
func createSessionWithToken(t *testing.T, ts *httptest.Server, sel map[string]any, anon string) (string, string) {
	t.Helper()
	body := map[string]any{}
	for k, v := range sel {
		body[k] = v
	}
	if anon != "" {
		body["anon_token"] = anon
	}
	code, res := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil, body)
	if code != http.StatusCreated {
		t.Fatalf("create session: %d %v", code, res)
	}
	id, _ := res["id"].(string)
	tok, _ := res["anon_token"].(string)
	if id == "" || tok == "" {
		t.Fatalf("sessão sem id/token: %v", res)
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

	// Sem token → 404; token errado (de outro navegador) → 404.
	if code, _ := do(t, ts, "GET", "/api/v1/public/checkout/sessions/"+id, nil, nil); code != http.StatusNotFound {
		t.Fatalf("sem anon_token: esperava 404, veio %d", code)
	}
	hdr := map[string]string{"X-Anon-Token": "errado"}
	if code, _ := do(t, ts, "GET", "/api/v1/public/checkout/sessions/"+id, hdr, nil); code != http.StatusNotFound {
		t.Fatalf("anon_token de outro navegador: esperava 404, veio %d", code)
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

	// PATCH e auth-started seguem a mesma regra (404 para token errado/ausente).
	if code, _ := do(t, ts, "PATCH", "/api/v1/public/checkout/sessions/"+id, map[string]string{"X-Anon-Token": "errado"},
		map[string]any{"quantity": 1}); code != http.StatusNotFound {
		t.Fatalf("PATCH com token errado: esperava 404, veio %d", code)
	}
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+id+"/auth-started", map[string]string{"X-Anon-Token": "errado"}, nil); code != http.StatusNotFound {
		t.Fatalf("auth-started com token errado: esperava 404, veio %d", code)
	}
}

// TestAnonStopsAfterBind: após o bind, o anon_token não dá mais acesso; só o subject dono
// segue no pagamento.
func TestAnonStopsAfterBind(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa StopAnon", "owner@stopanon.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show StopAnon", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	id, tok := createSession(t, ts, map[string]any{"event_id": eventID, "quantity": 1})

	ana := buyer(t, ts, pool, "stop@x.com")
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+id+"/bind", ana, nil); code != http.StatusOK {
		t.Fatalf("bind: %d", code)
	}
	// anon_token não acessa mais (sessão authenticated).
	if code, _ := do(t, ts, "GET", "/api/v1/public/checkout/sessions/"+id, map[string]string{"X-Anon-Token": tok}, nil); code != http.StatusNotFound {
		t.Fatalf("anon após bind: esperava 404, veio %d", code)
	}
	// O subject dono segue para o pagamento.
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+id+"/pay", ana, map[string]any{"method": "pix"}); code != http.StatusCreated {
		t.Fatalf("pay do dono: %d", code)
	}
}

// TestSessionCapPerIP: o teto de sessões por IP é grosseiro (50) — a 51ª é recusada.
func TestSessionCapPerIP(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducer(t, ts, "Casa Cap", "owner@cap.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show Cap", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 200)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	lim := checkout.DefaultLimits()
	for i := 0; i < lim.MaxOpenSessionsPerIP; i++ {
		code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil, map[string]any{"event_id": eventID, "quantity": 1})
		if code != http.StatusCreated {
			t.Fatalf("sessão %d: %d", i, code)
		}
	}
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil, map[string]any{"event_id": eventID, "quantity": 1}); code != http.StatusTooManyRequests {
		t.Fatalf("teto por IP: esperava 429, veio %d", code)
	}
}

// TestSeatedIPHeldSeatsLimit: evento com assento marcado recusa reserva acima de
// MaxHeldSeatsPerIP no mesmo IP.
func TestSeatedIPHeldSeatsLimit(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Seats", "owner@seats.com", "senha1234")
	pid := producerID(t, ts, owner)
	eventID, seats, _ := seatedEvent(t, ts, pool, owner, pid, 40)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	lim := checkout.DefaultLimits()
	// Reserva assentos 1 a 1 até encher o teto; a próxima é recusada (429).
	for i := 0; i < lim.MaxHeldSeatsPerIP; i++ {
		code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil, map[string]any{
			"event_id": eventID.String(), "quantity": 1, "seat_ids": []string{seats[i].String()},
		})
		if code != http.StatusCreated {
			t.Fatalf("sessão %d: %d", i, code)
		}
	}
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil, map[string]any{
		"event_id": eventID.String(), "quantity": 1, "seat_ids": []string{seats[lim.MaxHeldSeatsPerIP].String()},
	}); code != http.StatusTooManyRequests {
		t.Fatalf("teto de assentos por IP: esperava 429, veio %d", code)
	}
}

// TestStandingIgnoresHeldSeatsLimit: evento em pé não aplica limite de assentos por IP.
func TestStandingIgnoresHeldSeatsLimit(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducer(t, ts, "Casa Pista", "owner@pista.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show Pista", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 500)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	// Mais do que MaxHeldSeatsPerIP sessões em pé do mesmo IP: todas passam (sem limite).
	lim := checkout.DefaultLimits()
	for i := 0; i < lim.MaxHeldSeatsPerIP+5; i++ {
		code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil, map[string]any{"event_id": eventID, "quantity": 1})
		if code != http.StatusCreated {
			t.Fatalf("sessão pista %d: %d", i, code)
		}
	}
}

// TestResumeSameEventReturnsExisting: criar sessão com anon_token que já tem sessão open no
// mesmo evento devolve a existente, sem criar outra.
func TestResumeSameEventReturnsExisting(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Resume", "owner@resume2.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Resume2", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	id1, _ := createSessionWithToken(t, ts, map[string]any{"event_id": eventID, "quantity": 1}, "tok-resume")
	id2, _ := createSessionWithToken(t, ts, map[string]any{"event_id": eventID, "quantity": 1}, "tok-resume")
	if id1 != id2 {
		t.Fatalf("retomada deveria devolver a mesma sessão: %s vs %s", id1, id2)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM checkout_sessions WHERE anon_token='tok-resume'`); n != 1 {
		t.Fatalf("esperava 1 sessão, veio %d", n)
	}
}

// TestCreateExpiresOwnOtherEvent: criar sessão expira as 'open' do mesmo anon_token em outro
// evento e libera as reservas.
func TestCreateExpiresOwnOtherEvent(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Own", "owner@own.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	evA := createEvent(t, ts, owner, "Show A", "shows")
	lotA := createLot(t, ts, owner, evA, "Lote 1", 5000, 100, 0)
	_ = createLot(t, ts, owner, evA, "Lote 2", 7000, 100, 1)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+evA+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	evB := createEvent(t, ts, owner, "Show B", "shows")
	_ = createLot(t, ts, owner, evB, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+evB+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}

	// Sessão open no evento A reserva 2 do lote.
	createSessionWithToken(t, ts, map[string]any{"event_id": evA, "quantity": 2}, "tok-own")
	if h := scanInt(t, ctx, pool, pid, `SELECT held_count FROM lots WHERE id=$1`, uuid.MustParse(lotA)); h != 2 {
		t.Fatalf("esperava held 2, veio %d", h)
	}

	// Criar sessão no evento B com o mesmo anon_token expira a do evento A e libera o lote.
	createSessionWithToken(t, ts, map[string]any{"event_id": evB, "quantity": 1}, "tok-own")
	if st := scanStr(t, ctx, pool, pid, `SELECT status FROM checkout_sessions WHERE event_id=$1`, uuid.MustParse(evA)); st != "expired" {
		t.Fatalf("sessão do evento A deveria estar expirada, veio %s", st)
	}
	if h := scanInt(t, ctx, pool, pid, `SELECT held_count FROM lots WHERE id=$1`, uuid.MustParse(lotA)); h != 0 {
		t.Fatalf("lote do evento A deveria ser liberado, veio %d", h)
	}
}

// TestAuthStartedExtendsOnce: auth-started estende reserva e sessão UMA vez; a segunda
// chamada não estende, e o bind depois não estende de novo.
func TestAuthStartedExtendsOnce(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Auth", "owner@auth.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, seats, _ := seatedEvent(t, ts, pool, owner, pid, 1)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	id, tok := createSession(t, ts, map[string]any{
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
	hdr := map[string]string{"X-Anon-Token": tok}
	base := readExp()
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+id+"/auth-started", hdr, nil); code != http.StatusOK {
		t.Fatalf("auth-started: %d", code)
	}
	after1 := readExp()
	if after1.Sub(base) < 30*time.Second {
		t.Fatalf("auth-started deveria estender a reserva (base=%v after=%v)", base, after1)
	}
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+id+"/auth-started", hdr, nil); code != http.StatusOK {
		t.Fatalf("auth-started 2: %d", code)
	}
	if after2 := readExp(); !after2.Equal(after1) {
		t.Fatalf("segunda chamada não deveria estender (after1=%v after2=%v)", after1, after2)
	}
	// Bind após auth-started não estende de novo (grace já aplicado).
	ana := buyer(t, ts, pool, "auth@x.com")
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+id+"/bind", ana, nil); code != http.StatusOK {
		t.Fatalf("bind: %d", code)
	}
	if after3 := readExp(); !after3.Equal(after1) {
		t.Fatalf("bind não deveria estender de novo (after1=%v after3=%v)", after1, after3)
	}
}

// TestSweeperClearsClientIP: sessão expirada pelo sweeper fica com client_ip nulo.
func TestSweeperClearsClientIP(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa IP", "owner@ip.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show IP", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	id, _ := createSession(t, ts, map[string]any{"event_id": eventID, "quantity": 1})
	if ip := scanStr(t, ctx, pool, pid, `SELECT COALESCE(client_ip,'') FROM checkout_sessions WHERE id=$1`, uuid.MustParse(id)); ip == "" {
		t.Fatalf("client_ip deveria estar preenchido")
	}
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `UPDATE checkout_sessions SET expires_at=now()-interval '1 minute' WHERE id=$1`, uuid.MustParse(id)); err != nil {
			t.Fatalf("expirar: %v", err)
		}
		if _, err := checkout.ExpireOpenSessions(ctx, tx); err != nil {
			t.Fatalf("expire: %v", err)
		}
	})
	if ip := scanStr(t, ctx, pool, pid, `SELECT COALESCE(client_ip,'') FROM checkout_sessions WHERE id=$1`, uuid.MustParse(id)); ip != "" {
		t.Fatalf("client_ip deveria ser limpo ao expirar, veio %q", ip)
	}
}

// TestPaidIPPurgedAfterRetention: sessão paid tem client_ip apagado após a retenção.
func TestPaidIPPurgedAfterRetention(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa RetIP", "owner@retip.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show RetIP", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	id, tok := createSession(t, ts, map[string]any{"event_id": eventID, "quantity": 1})
	_ = tok
	// Marca como paid e envelhece além da retenção.
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `UPDATE checkout_sessions SET status='paid', updated_at=now()-interval '10 days' WHERE id=$1`, uuid.MustParse(id)); err != nil {
			t.Fatalf("paid: %v", err)
		}
		if err := checkout.PurgePaidIPs(ctx, tx, checkout.DefaultLimits().IPRetention); err != nil {
			t.Fatalf("purge: %v", err)
		}
	})
	if ip := scanStr(t, ctx, pool, pid, `SELECT COALESCE(client_ip,'') FROM checkout_sessions WHERE id=$1`, uuid.MustParse(id)); ip != "" {
		t.Fatalf("client_ip deveria ser apagado após a retenção, veio %q", ip)
	}
}

// TestAuthenticatedExpiresToAbandoned: sessão vinculada que expira vira 'abandoned',
// libera a reserva e limpa o client_ip.
func TestAuthenticatedExpiresToAbandoned(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Aban", "owner@aban.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, seats, _ := seatedEvent(t, ts, pool, owner, pid, 1)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	id, _ := createSession(t, ts, map[string]any{
		"event_id": eventID.String(), "quantity": 1, "seat_ids": []string{seats[0].String()},
	})
	ana := buyer(t, ts, pool, "aban@x.com")
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+id+"/bind", ana, nil); code != http.StatusOK {
		t.Fatalf("bind: %d", code)
	}
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `UPDATE checkout_sessions SET expires_at=now()-interval '1 minute' WHERE id=$1`, uuid.MustParse(id)); err != nil {
			t.Fatalf("expirar: %v", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE seat_occupancy SET expires_at=now()-interval '1 minute' WHERE seat_id=$1 AND kind='hold'`, seats[0]); err != nil {
			t.Fatalf("expirar ocupação: %v", err)
		}
		if _, err := checkout.ExpireOpenSessions(ctx, tx); err != nil {
			t.Fatalf("expire: %v", err)
		}
	})
	if st := scanStr(t, ctx, pool, pid, `SELECT status FROM checkout_sessions WHERE id=$1`, uuid.MustParse(id)); st != "abandoned" {
		t.Fatalf("sessão vinculada deveria virar abandoned, veio %s", st)
	}
	if ip := scanStr(t, ctx, pool, pid, `SELECT COALESCE(client_ip,'') FROM checkout_sessions WHERE id=$1`, uuid.MustParse(id)); ip != "" {
		t.Fatalf("client_ip deveria ser limpo, veio %q", ip)
	}
	// Assento liberado.
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM seat_occupancy WHERE seat_id=$1 AND kind='hold' AND NOT released`, seats[0]); n != 0 {
		t.Fatalf("assento deveria ser liberado, veio %d", n)
	}
}

// TestOpenExpiresToExpired: sessão 'open' que expira continua virando 'expired'.
func TestOpenExpiresToExpired(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa OpExp", "owner@opexp.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show OpExp", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	id, _ := createSession(t, ts, map[string]any{"event_id": eventID, "quantity": 1})
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `UPDATE checkout_sessions SET expires_at=now()-interval '1 minute' WHERE id=$1`, uuid.MustParse(id)); err != nil {
			t.Fatalf("expirar: %v", err)
		}
		if _, err := checkout.ExpireOpenSessions(ctx, tx); err != nil {
			t.Fatalf("expire: %v", err)
		}
	})
	if st := scanStr(t, ctx, pool, pid, `SELECT status FROM checkout_sessions WHERE id=$1`, uuid.MustParse(id)); st != "expired" {
		t.Fatalf("sessão open deveria virar expired, veio %s", st)
	}
}

// TestSessionFunnelInOverview: a métrica de vinculadas contra pagas aparece no painel.
func TestSessionFunnelInOverview(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Funil", "owner@funil.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Funil", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}

	// Uma compra paga.
	body := buyViaSession(t, ts, buyer(t, ts, pool, "paga@funil.com"), map[string]any{"event_id": eventID, "quantity": 1}, "pix")
	confirmWebhook(t, ts, body["asaas_ref"].(string))

	// Uma vinculada que abandonou (bind + expira).
	id, _ := createSession(t, ts, map[string]any{"event_id": eventID, "quantity": 1})
	ana := buyer(t, ts, pool, "abandona@funil.com")
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions/"+id+"/bind", ana, nil); code != http.StatusOK {
		t.Fatalf("bind: %d", code)
	}
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `UPDATE checkout_sessions SET expires_at=now()-interval '1 minute' WHERE id=$1`, uuid.MustParse(id)); err != nil {
			t.Fatalf("expirar: %v", err)
		}
		if _, err := checkout.ExpireOpenSessions(ctx, tx); err != nil {
			t.Fatalf("expire: %v", err)
		}
	})

	code, body := do(t, ts, "GET", "/api/v1/dash/events/"+eventID, bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("dash overview: %d", code)
	}
	funnel := body["session_funnel"].(map[string]any)
	paid := funnel["paid"].(float64)
	abandoned := funnel["abandoned"].(float64)
	bound := funnel["bound"].(float64)
	if paid < 1 || abandoned < 1 || bound != paid+abandoned {
		t.Fatalf("funil inesperado: %v", funnel)
	}
}
// TestSessionResponseNoClientIP: nenhuma resposta de API expõe client_ip.
func TestSessionResponseNoClientIP(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducer(t, ts, "Casa NoIP", "owner@noip.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show NoIP", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	code, body := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil, map[string]any{"event_id": eventID, "quantity": 1})
	if code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if _, has := body["client_ip"]; has {
		t.Fatalf("resposta não deveria expor client_ip: %v", body)
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
