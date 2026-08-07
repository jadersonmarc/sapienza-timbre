package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-timbre/internal/catalog"
)

// createLot cria um lote via API e devolve o id.
func createLot(t *testing.T, ts *httptest.Server, token, eventID, name string, price int64, stock, position int) string {
	t.Helper()
	code, body := do(t, ts, "POST", "/api/v1/events/"+eventID+"/lots", bearer(token),
		map[string]any{"name": name, "price_cents": price, "stock": stock, "position": position})
	if code != http.StatusCreated {
		t.Fatalf("criar lote %s: status %d, body %v", name, code, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("lote sem id: %v", body)
	}
	return id
}

// createEvent cria um evento via API e devolve o id.
func createEvent(t *testing.T, ts *httptest.Server, token, title, category string) string {
	t.Helper()
	code, body := do(t, ts, "POST", "/api/v1/events", bearer(token),
		map[string]any{"title": title, "category": category})
	if code != http.StatusCreated {
		t.Fatalf("criar evento: status %d, body %v", code, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("evento sem id: %v", body)
	}
	return id
}

// TestCatalogEventThreeLotsRollover é o "pronto quando" da Etapa 1.2: um produtor
// cadastra um evento com três lotes e a virada automática ao esgotar o estoque é
// coberta por teste.
func TestCatalogEventThreeLotsRollover(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Cat", "owner@cat.com", "senha1234")
	ctx := context.Background()
	pid := producerID(t, ts, owner)

	eventID := createEvent(t, ts, owner, "Show X", "shows")

	// Três lotes, estoque 2 cada, em ordem de posição.
	l0 := uuid.MustParse(createLot(t, ts, owner, eventID, "Lote 1", 5000, 2, 0))
	l1 := uuid.MustParse(createLot(t, ts, owner, eventID, "Lote 2", 7000, 2, 1))
	l2 := uuid.MustParse(createLot(t, ts, owner, eventID, "Lote 3", 9000, 2, 2))

	// Publicar ativa o lote inicial.
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: status %d", code)
	}
	assertLotStatus(t, ctx, pool, pid, l0, "active")
	assertLotStatus(t, ctx, pool, pid, l1, "scheduled")
	assertLotStatus(t, ctx, pool, pid, l2, "scheduled")

	// Esgota o lote 1 → sold_out e o lote 2 vira active automaticamente.
	lot := sell(t, ctx, pool, pid, l0, 2)
	if lot.Status != "sold_out" {
		t.Fatalf("lote 1 após esgotar: esperava sold_out, veio %s", lot.Status)
	}
	assertLotStatus(t, ctx, pool, pid, l0, "sold_out")
	assertLotStatus(t, ctx, pool, pid, l1, "active")
	assertLotStatus(t, ctx, pool, pid, l2, "scheduled")

	// Esgota o lote 2 → lote 3 vira active.
	sell(t, ctx, pool, pid, l1, 2)
	assertLotStatus(t, ctx, pool, pid, l1, "sold_out")
	assertLotStatus(t, ctx, pool, pid, l2, "active")

	// Esgota o lote 3 → sem próximo (fim da fila).
	sell(t, ctx, pool, pid, l2, 2)
	assertLotStatus(t, ctx, pool, pid, l2, "sold_out")

	// Vender de um lote não ativo → ErrLotNotSellable.
	if _, err := sellErr(t, ctx, pool, pid, l0, 1); err != catalog.ErrLotNotSellable {
		t.Fatalf("venda em lote esgotado: esperava ErrLotNotSellable, veio %v", err)
	}
}

// TestCatalogOverselling: a atomicidade do UPDATE impede vender além do estoque.
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

	// Estoque 3: vender 2 ok, depois 2 falha (só resta 1).
	sell(t, ctx, pool, pid, l0, 2)
	if _, err := sellErr(t, ctx, pool, pid, l0, 2); err != catalog.ErrInsufficientStock {
		t.Fatalf("oversell: esperava ErrInsufficientStock, veio %v", err)
	}
	// Vender exatamente o que resta esgota (lote único, sem virada).
	lot := sell(t, ctx, pool, pid, l0, 1)
	if lot.Status != "sold_out" {
		t.Fatalf("último item: esperava sold_out, veio %s", lot.Status)
	}
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

func sell(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid, lotID uuid.UUID, qty int) catalog.Lot {
	t.Helper()
	lot, err := sellErr(t, ctx, pool, pid, lotID, qty)
	if err != nil {
		t.Fatalf("sell lote %s: %v", lotID, err)
	}
	return lot
}

func sellErr(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid, lotID uuid.UUID, qty int) (catalog.Lot, error) {
	t.Helper()
	var lot catalog.Lot
	var sErr error
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		lot, sErr = catalog.SellFromLot(ctx, tx, lotID, qty)
	})
	return lot, sErr
}

func assertLotStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid, lotID uuid.UUID, want string) {
	t.Helper()
	var l catalog.Lot
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		var e error
		if l, e = catalog.GetLot(ctx, tx, lotID); e != nil {
			t.Fatalf("get lote %s: %v", lotID, e)
		}
	})
	if l.Status != want {
		t.Fatalf("lote %s: esperava status %s, veio %s", lotID, want, l.Status)
	}
}
