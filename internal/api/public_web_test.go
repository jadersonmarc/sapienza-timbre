package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
)

// publishWithCity cria produtor+evento+lote, define a cidade e publica. Devolve
// (ownerToken, eventID, primeiro lotID).
func publishWithCity(t *testing.T, ts *httptest.Server, name, email, city, category string, price int64) (string, string) {
	t.Helper()
	_, owner := createProducer(t, ts, name, email, "senha1234")
	eventID := createEvent(t, ts, owner, "Show "+name, category)
	_ = createLot(t, ts, owner, eventID, "Lote 1", price, 100, 0)
	if code, _ := do(t, ts, "PATCH", "/api/v1/events/"+eventID, bearer(owner), map[string]any{"city": city}); code != http.StatusOK {
		t.Fatalf("patch city: %d", code)
	}
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publish: %d", code)
	}
	return owner, eventID
}

// TestPublicDirectoryAndDetail: diretório lista o publicado (com cidade e preço mínimo), o
// detalhe traz lotes/lote corrente sem campo de owner, e suspender tira do diretório.
func TestPublicDirectoryAndDetail(t *testing.T) {
	ts, _ := setup(t)
	owner, eventID := publishWithCity(t, ts, "CasaDir", "owner@dir.com", "São Paulo", "shows", 5000)

	code, body := do(t, ts, "GET", "/api/v1/public/events?q=Show&city=São", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("diretório: %d", code)
	}
	events, _ := body["events"].([]any)
	found := false
	for _, e := range events {
		m := e.(map[string]any)
		if m["event_id"] == eventID {
			found = true
			if m["city"] != "São Paulo" {
				t.Fatalf("card sem cidade: %v", m)
			}
			if m["min_price_cents"].(float64) != 5000 {
				t.Fatalf("min_price esperado 5000, veio %v", m["min_price_cents"])
			}
		}
	}
	if !found {
		t.Fatalf("evento publicado não apareceu no diretório: %v", events)
	}

	// Detalhe público: evento + lotes + lote corrente; sem chave de produtor/financeiro.
	code, det := do(t, ts, "GET", "/api/v1/public/events/"+eventID, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("detalhe: %d", code)
	}
	if det["current_lot_id"] == nil {
		t.Fatalf("detalhe sem lote corrente: %v", det)
	}
	if _, leaked := det["retention_pct"]; leaked {
		t.Fatalf("detalhe público vazou campo de owner")
	}
	ev := det["event"].(map[string]any)
	if ev["title"] == nil || ev["category"] != "shows" {
		t.Fatalf("detalhe incompleto: %v", ev)
	}

	// Suspender remove do diretório.
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/suspend", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("suspend: %d", code)
	}
	_, body2 := do(t, ts, "GET", "/api/v1/public/events", nil, nil)
	for _, e := range asSlice(body2["events"]) {
		if e.(map[string]any)["event_id"] == eventID {
			t.Fatalf("evento suspenso continua no diretório")
		}
	}
}

// TestPublicMinPriceResyncOnRollover: esgotado o lote 1, o min_price do diretório passa a
// ser o do lote 2 (§3.10) — via o caminho real de checkout+webhook.
func TestPublicMinPriceResyncOnRollover(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "CasaMin", "owner@min.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show Min", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 1, 0) // 1 unidade
	_ = createLot(t, ts, owner, eventID, "Lote 2", 7000, 100, 1)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publish: %d", code)
	}
	if mp := directoryMinPrice(t, ts); mp != 5000 {
		t.Fatalf("min_price inicial esperado 5000, veio %d", mp)
	}

	// Compra a única unidade do lote 1 (resolve o corrente) e confirma.
	code, res := do(t, ts, "POST", "/api/v1/public/checkout", buyer(t, ts, pool, "c@min.com"), map[string]any{
		"event_id": eventID, "quantity": 1, "method": "pix",
	})
	if code != http.StatusCreated {
		t.Fatalf("checkout: %d %v", code, res)
	}
	confirmWebhook(t, ts, res["asaas_ref"].(string))

	if mp := directoryMinPrice(t, ts); mp != 7000 {
		t.Fatalf("após esgotar o lote 1, min_price esperado 7000, veio %d", mp)
	}
}

// TestBuyerMustBeAuthedToBuy: compra exige cadastro — sem token → 401; autenticado, o
// comprador compra e vê o ingresso em "meus ingressos"; outro comprador não enxerga o
// ingresso alheio (IDOR/escopo).
func TestBuyerMustBeAuthedToBuy(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "CasaOtp", "owner@otp.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show Otp", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publish: %d", code)
	}

	// Sem sessão → recusado.
	code, _ := do(t, ts, "POST", "/api/v1/public/checkout", nil, map[string]any{
		"event_id": eventID, "quantity": 1, "method": "pix",
	})
	if code != http.StatusUnauthorized {
		t.Fatalf("checkout sem sessão: esperava 401, veio %d", code)
	}

	// O comprador entra ANTES de comprar.
	token := verifyOTP(t, ts, pool, "convidada@x.com", "123456")
	code, res := do(t, ts, "POST", "/api/v1/public/checkout", bearer(token), map[string]any{
		"event_id": eventID, "quantity": 1, "method": "pix",
	})
	if code != http.StatusCreated {
		t.Fatalf("checkout: %d %v", code, res)
	}
	confirmWebhook(t, ts, res["asaas_ref"].(string))

	code, mine := do(t, ts, "GET", "/api/v1/public/me/tickets", bearer(token), nil)
	if code != http.StatusOK {
		t.Fatalf("me/tickets: %d", code)
	}
	if len(asSlice(mine["tickets"])) != 1 {
		t.Fatalf("esperava 1 ingresso vinculado, veio %v", mine["tickets"])
	}

	// Outro comprador não enxerga o ingresso alheio (IDOR/escopo).
	other := verifyOTP(t, ts, pool, "outro@x.com", "654321")
	_, otherMine := do(t, ts, "GET", "/api/v1/public/me/tickets", bearer(other), nil)
	if len(asSlice(otherMine["tickets"])) != 0 {
		t.Fatalf("comprador viu ingresso de outro: %v", otherMine["tickets"])
	}
}

// TestOtpWrongCodeAndNeutralResponse: código errado é recusado; request-code é neutro.
func TestOtpWrongCodeAndNeutralResponse(t *testing.T) {
	ts, pool := setup(t)
	// request-code sempre 200 neutro (não revela cadastro).
	if code, _ := do(t, ts, "POST", "/api/v1/public/auth/request-code", nil, map[string]any{"email": "qualquer@x.com"}); code != http.StatusOK {
		t.Fatalf("request-code: %d", code)
	}
	insertOTP(t, pool, "verify@x.com", "111111")
	if code, _ := do(t, ts, "POST", "/api/v1/public/auth/verify", nil, map[string]any{"email": "verify@x.com", "code": "000000"}); code != http.StatusUnauthorized {
		t.Fatalf("código errado: esperava 401, veio %d", code)
	}
}

// TestBuyerTokenRejectedOnProducerRoute: token de comprador não vale em rota de produtor.
func TestBuyerTokenRejectedOnProducerRoute(t *testing.T) {
	ts, pool := setup(t)
	token := verifyOTP(t, ts, pool, "escopo@x.com", "222222")
	if code, _ := do(t, ts, "GET", "/api/v1/events", bearer(token), nil); code != http.StatusUnauthorized {
		t.Fatalf("token de comprador em rota de produtor: esperava 401, veio %d", code)
	}
}

// TestProducerSignupActive: cadastro público cria produtor ATIVO na hora (self-service)
// e já devolve o token do owner.
func TestProducerSignupActive(t *testing.T) {
	ts, pool := setup(t)
	code, body := do(t, ts, "POST", "/api/v1/public/producer-signup", nil, map[string]any{
		"name": "Nova Casa", "owner_email": "novo@casa.com", "owner_password": "senha1234",
	})
	if code != http.StatusCreated {
		t.Fatalf("signup: %d %v", code, body)
	}
	prod := body["producer"].(map[string]any)
	if prod["status"] != "active" {
		t.Fatalf("produtor deveria nascer active, veio %v", prod["status"])
	}
	if _, ok := body["token"].(string); !ok {
		t.Fatalf("signup deveria devolver token do owner: %v", body)
	}
	// E consta ativo no control plane.
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM producers WHERE id=$1`, prod["id"]).Scan(&status); err != nil {
		t.Fatalf("ler status: %v", err)
	}
	if status != "active" {
		t.Fatalf("status no banco: %s", status)
	}
}

// TestPublicQuoteDecomposition (§4): a cotação devolve a decomposição face + conveniência
// + total, sem criar ordem.
func TestPublicQuoteDecomposition(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducer(t, ts, "CasaQuote", "owner@quote.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show Quote", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publish: %d", code)
	}
	code, bd := do(t, ts, "POST", "/api/v1/public/checkout/quote", nil, map[string]any{
		"event_id": eventID, "quantity": 2, "method": "pix",
	})
	if code != http.StatusOK {
		t.Fatalf("quote: %d %v", code, bd)
	}
	if bd["face_cents"].(float64) != 10000 || bd["convenience_fee_cents"].(float64) != 900 || bd["total_cents"].(float64) != 10900 {
		t.Fatalf("decomposição inesperada: %v", bd)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// insertOTP grava um código de acesso conhecido (hash bcrypt) para o e-mail.
func insertOTP(t *testing.T, pool *pgxpool.Pool, email, code string) {
	t.Helper()
	hash, err := auth.HashPassword(code)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO buyer_otps (email, code_hash, expires_at)
		VALUES ($1, $2, now() + interval '10 minutes')`, email, hash); err != nil {
		t.Fatalf("inserir otp: %v", err)
	}
}

// verifyOTP insere um código e chama /verify, devolvendo o token do comprador.
func verifyOTP(t *testing.T, ts *httptest.Server, pool *pgxpool.Pool, email, code string) string {
	t.Helper()
	insertOTP(t, pool, email, code)
	c, body := do(t, ts, "POST", "/api/v1/public/auth/verify", nil, map[string]any{"email": email, "code": code})
	if c != http.StatusOK {
		t.Fatalf("verify %s: %d %v", email, c, body)
	}
	tok, _ := body["token"].(string)
	if tok == "" {
		t.Fatalf("verify sem token: %v", body)
	}
	return tok
}

// directoryMinPrice devolve o menor min_price presente no diretório (há 1 evento nos testes).
func directoryMinPrice(t *testing.T, ts *httptest.Server) int {
	t.Helper()
	_, body := do(t, ts, "GET", "/api/v1/public/events", nil, nil)
	events := asSlice(body["events"])
	if len(events) == 0 {
		t.Fatalf("diretório vazio")
	}
	return int(events[0].(map[string]any)["min_price_cents"].(float64))
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}
