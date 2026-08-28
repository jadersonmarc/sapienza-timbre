package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-timbre/internal/catalog"
)

// createLot cria um lote via API e devolve o id. Os dois inteiros são quantity e
// sort_order (no modelo de contadores).
func createLot(t *testing.T, ts *httptest.Server, token, eventID, name string, price int64, quantity, sortOrder int) string {
	t.Helper()
	code, body := do(t, ts, "POST", "/api/v1/events/"+eventID+"/lots", bearer(token),
		map[string]any{"name": name, "price_cents": price, "quantity": quantity, "sort_order": sortOrder})
	if code != http.StatusCreated {
		t.Fatalf("criar lote %s: status %d, body %v", name, code, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("lote sem id: %v", body)
	}
	return id
}

// createEvent cria um evento via API e devolve o id. Já nasce com data de início futura
// (publicação exige data futura no modelo reconciliado da 1.2).
func createEvent(t *testing.T, ts *httptest.Server, token, title, category string) string {
	t.Helper()
	startsAt := time.Now().Add(30 * 24 * time.Hour).Format(time.RFC3339)
	code, body := do(t, ts, "POST", "/api/v1/events", bearer(token),
		map[string]any{"title": title, "category": category, "starts_at": startsAt})
	if code != http.StatusCreated {
		t.Fatalf("criar evento: status %d, body %v", code, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("evento sem id: %v", body)
	}
	return id
}

// TestCatalogEventThreeLotsRollover é o "pronto quando" da Etapa 1.2 no modelo
// derivado: três lotes; a virada por ESGOTAMENTO é apenas resolver o lote corrente
// (CurrentLot) — nenhum estado de status é escrito.
func TestCatalogEventThreeLotsRollover(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Cat", "owner@cat.com", "senha1234")
	ctx := context.Background()
	pid := producerID(t, ts, owner)

	eventID := createEvent(t, ts, owner, "Show X", "shows")
	eid := uuid.MustParse(eventID)

	// Três lotes, capacidade 2 cada, em ordem de sort_order.
	l0 := uuid.MustParse(createLot(t, ts, owner, eventID, "Lote 1", 5000, 2, 0))
	l1 := uuid.MustParse(createLot(t, ts, owner, eventID, "Lote 2", 7000, 2, 1))
	l2 := uuid.MustParse(createLot(t, ts, owner, eventID, "Lote 3", 9000, 2, 2))

	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: status %d", code)
	}

	// O corrente é o lote 1.
	assertCurrentLot(t, ctx, pool, pid, eid, l0)

	// Confirma 2 no lote 1 (Reserve+Confirm) → esgota → corrente vira o lote 2.
	confirmSale(t, ctx, pool, pid, eid, l0, 2)
	assertCurrentLot(t, ctx, pool, pid, eid, l1)

	// Esgota o lote 2 → corrente vira o lote 3.
	confirmSale(t, ctx, pool, pid, eid, l1, 2)
	assertCurrentLot(t, ctx, pool, pid, eid, l2)

	// Esgota o lote 3 → sem corrente (fim da fila).
	confirmSale(t, ctx, pool, pid, eid, l2, 2)
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if _, err := catalog.CurrentLot(ctx, tx, eid); err != catalog.ErrNoCurrentLot {
			t.Fatalf("após esgotar tudo: esperava ErrNoCurrentLot, veio %v", err)
		}
	})
}

// TestCatalogOverselling: o UPDATE condicional de ReserveFromLot impede segurar além da
// capacidade; o CHECK lots_capacity_chk nunca é violado.
func TestCatalogOverselling(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Over", "owner@over.com", "senha1234")
	ctx := context.Background()
	pid := producerID(t, ts, owner)

	eventID := createEvent(t, ts, owner, "Peça", "teatro")
	l0 := uuid.MustParse(createLot(t, ts, owner, eventID, "Único", 3000, 3, 0))
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}

	// Capacidade 3: reservar 2 ok; reservar mais 2 falha (só resta 1).
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := catalog.ReserveFromLot(ctx, tx, l0, 2); err != nil {
			t.Fatalf("reservar 2: %v", err)
		}
		if err := catalog.ReserveFromLot(ctx, tx, l0, 2); err != catalog.ErrInsufficientStock {
			t.Fatalf("oversell: esperava ErrInsufficientStock, veio %v", err)
		}
		// A última unidade cabe.
		if err := catalog.ReserveFromLot(ctx, tx, l0, 1); err != nil {
			t.Fatalf("reservar última: %v", err)
		}
		var sold, held, qty int
		if err := tx.QueryRow(ctx, `SELECT sold_count, held_count, quantity FROM lots WHERE id=$1`, l0).Scan(&sold, &held, &qty); err != nil {
			t.Fatalf("ler contadores: %v", err)
		}
		if sold+held != qty {
			t.Fatalf("capacidade: sold+held=%d, quantity=%d", sold+held, qty)
		}
	})
}

// TestCatalogInvalidTransition: transição de ciclo de vida inválida é recusada.
func TestCatalogInvalidTransition(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Ciclo", "owner@ciclo2.com", "senha1234")
	ctx := context.Background()
	pid := producerID(t, ts, owner)
	eventID := uuid.MustParse(createEvent(t, ts, owner, "Show Ciclo", "shows"))

	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		// draft→finished não é permitida.
		if err := catalog.TransitionEvent(ctx, tx, eventID, "finished"); !errors.Is(err, catalog.ErrInvalidTransition) {
			t.Fatalf("draft→finished: esperava ErrInvalidTransition, veio %v", err)
		}
		// draft→pending_review é permitida.
		if err := catalog.TransitionEvent(ctx, tx, eventID, "pending_review"); err != nil {
			t.Fatalf("draft→pending_review: %v", err)
		}
	})
}

// TestCatalogWriteRequiresOwner: escrita no catálogo é do owner; leitura, de qualquer
// colaborador autenticado.
func TestCatalogWriteRequiresOwner(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducer(t, ts, "Casa Perm", "owner@perm.com", "senha1234")
	if code, _ := do(t, ts, "POST", "/api/v1/collaborators", bearer(owner),
		map[string]any{"email": "rel@perm.com", "password": "senha1234", "permissions": []string{"relatorios"}}); code != http.StatusCreated {
		t.Fatalf("criar colaborador: %d", code)
	}
	relToken := login(t, ts, "rel@perm.com", "senha1234")
	if code, _ := do(t, ts, "POST", "/api/v1/events", bearer(relToken),
		map[string]any{"title": "X", "category": "festas"}); code != http.StatusForbidden {
		t.Fatalf("não-owner criar evento: esperava 403, veio %d", code)
	}
	if code, _ := do(t, ts, "GET", "/api/v1/events", bearer(relToken), nil); code != http.StatusOK {
		t.Fatalf("colaborador listar eventos: esperava 200, veio %d", code)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// producerID lê o producer id do /me do token.
func producerID(t *testing.T, ts *httptest.Server, token string) uuid.UUID {
	t.Helper()
	code, body := do(t, ts, "GET", "/api/v1/me", bearer(token), nil)
	if code != http.StatusOK {
		t.Fatalf("/me: %d", code)
	}
	collab, _ := body["collaborator"].(map[string]any)
	pid, _ := collab["producer_id"].(string)
	return uuid.MustParse(pid)
}

// confirmSale simula uma venda completa de pista no lote: reserva e confirma `qty`
// (held→sold), numa transação. Espelha o caminho do checkout no modelo de contadores.
func confirmSale(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid, _, lotID uuid.UUID, qty int) {
	t.Helper()
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := catalog.ReserveFromLot(ctx, tx, lotID, qty); err != nil {
			t.Fatalf("reservar %d no lote %s: %v", qty, lotID, err)
		}
		if _, err := catalog.ConfirmFromLot(ctx, tx, lotID, qty); err != nil {
			t.Fatalf("confirmar %d no lote %s: %v", qty, lotID, err)
		}
	})
}

// assertCurrentLot verifica que o lote vigente do evento é o esperado.
func assertCurrentLot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid, eventID, want uuid.UUID) {
	t.Helper()
	var got catalog.Lot
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		var e error
		if got, e = catalog.CurrentLot(ctx, tx, eventID); e != nil {
			t.Fatalf("lote vigente do evento %s: %v", eventID, e)
		}
	})
	if got.ID != want {
		t.Fatalf("lote vigente: esperava %s, veio %s", want, got.ID)
	}
}

// TestCupomDoProdutor: o produtor cria cupom por porcentagem ou por valor, e o comprador o
// usa no checkout. O desconto sai do FACE — quem dá o desconto é a casa, não a plataforma.
func TestCupomDoProdutor(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Cupom", "owner@cupom.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()

	eventID := createEvent(t, ts, owner, "Show", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote", 10000, 50, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar")
	}

	if code, b := do(t, ts, "POST", "/api/v1/events/"+eventID+"/coupons", bearer(owner),
		map[string]any{"code": "ESTREIA", "discount_pct": 10, "max_uses": 2}); code != http.StatusCreated {
		t.Fatalf("criar cupom: %d %v", code, b)
	}

	// O produtor vê o cupom e o uso — é o que ele vem conferir.
	code, list := do(t, ts, "GET", "/api/v1/events/"+eventID+"/coupons", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("listar cupons: %d", code)
	}
	cs, _ := list["coupons"].([]any)
	if len(cs) != 1 {
		t.Fatalf("esperava 1 cupom, veio %v", list["coupons"])
	}

	// Compra com o cupom: o face cai 10%, e a conveniência acompanha o face menor.
	res := buyViaSession(t, ts, buyer(t, ts, pool, "buy@cupom.com"), map[string]any{
		"event_id": eventID, "quantity": 1, "coupon_code": "ESTREIA",
	}, "pix")
	confirmWebhook(t, ts, res["asaas_ref"].(string))

	if face := scanInt(t, ctx, pool, pid, `SELECT face_cents FROM orders WHERE event_id=$1`, uuid.MustParse(eventID)); face != 9000 {
		t.Fatalf("esperava face de 9000 (10%% off de 10000), veio %d", face)
	}
	// O uso é contado: sem isso o limite não significa nada.
	if n := scanInt(t, ctx, pool, pid, `SELECT uses FROM coupons WHERE code='ESTREIA'`); n != 1 {
		t.Fatalf("esperava 1 uso registrado, veio %d", n)
	}
}
