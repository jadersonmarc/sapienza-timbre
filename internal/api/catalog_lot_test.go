package api_test

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-timbre/internal/catalog"
)

// reserveTx roda catalog.ReserveFromLot numa transação própria e faz commit no sucesso.
func reserveTx(ctx context.Context, pool *pgxpool.Pool, pid, lotID uuid.UUID, qty int) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, pid); err != nil {
		return err
	}
	if err := catalog.ReserveFromLot(ctx, tx, lotID, qty); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TestLotReservationOneWinner (critério 1): N goroutines disputando a ÚLTIMA unidade do
// lote → exatamente uma vence; sold_count+held_count nunca passa de quantity (o CHECK
// lots_capacity_chk nunca é violado). Espelha o TestReservationOneWinner do inventory,
// no nível do LOTE.
func TestLotReservationOneWinner(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa LoteConc", "owner@loteconc.com", "senha1234")
	ctx := context.Background()
	pid := producerID(t, ts, owner)

	eventID := createEvent(t, ts, owner, "Show Conc", "shows")
	lotID := uuid.MustParse(createLot(t, ts, owner, eventID, "Único", 5000, 1, 0))

	const n = 24
	var wins int64
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := reserveTx(ctx, pool, pid, lotID, 1); err == nil {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("esperava exatamente 1 vencedor, veio %d", wins)
	}
	var sold, held, qty int
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT sold_count, held_count, quantity FROM lots WHERE id=$1`, lotID).Scan(&sold, &held, &qty); err != nil {
			t.Fatalf("ler contadores: %v", err)
		}
	})
	if held != 1 || sold+held > qty {
		t.Fatalf("contadores inconsistentes: sold=%d held=%d quantity=%d", sold, held, qty)
	}
}

// TestRefundFromLotFloors (critério 11): estorno/queima repetidos não levam sold_count a
// negativo — o piso GREATEST(...,0) segura em 0.
func TestRefundFromLotFloors(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Piso", "owner@piso.com", "senha1234")
	ctx := context.Background()
	pid := producerID(t, ts, owner)

	eventID := createEvent(t, ts, owner, "Show Piso", "shows")
	lotID := uuid.MustParse(createLot(t, ts, owner, eventID, "Único", 5000, 10, 0))

	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := catalog.ReserveFromLot(ctx, tx, lotID, 2); err != nil {
			t.Fatalf("reservar: %v", err)
		}
		if _, err := catalog.ConfirmFromLot(ctx, tx, lotID, 2); err != nil {
			t.Fatalf("confirmar: %v", err)
		}
		// Estorno duplicado: 2 e mais 2 (excede o vendido) → chão em 0, sem negativo.
		if err := catalog.RefundFromLot(ctx, tx, lotID, 2); err != nil {
			t.Fatalf("estornar 1: %v", err)
		}
		if err := catalog.RefundFromLot(ctx, tx, lotID, 2); err != nil {
			t.Fatalf("estornar 2 (duplicado): %v", err)
		}
		var sold int
		if err := tx.QueryRow(ctx, `SELECT sold_count FROM lots WHERE id=$1`, lotID).Scan(&sold); err != nil {
			t.Fatalf("ler sold_count: %v", err)
		}
		if sold != 0 {
			t.Fatalf("esperava sold_count com piso em 0, veio %d", sold)
		}
	})
}

// TestCategoryPatchSyncsDirectory (critério 10): PATCH de categoria atualiza, de forma
// consistente, events.category, events.category_id E public.event_directory.category
// (escritor único applyCategory).
func TestCategoryPatchSyncsDirectory(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Categoria", "owner@cat3.com", "senha1234")
	ctx := context.Background()
	pid := producerID(t, ts, owner)

	eventID := createEvent(t, ts, owner, "Show Cat", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 10, 0)

	// PATCH troca a categoria de 'shows' para 'teatro' ENQUANTO em rascunho (permitido).
	if code, _ := do(t, ts, "PATCH", "/api/v1/events/"+eventID, bearer(owner),
		map[string]any{"category": "teatro"}); code != http.StatusOK {
		t.Fatalf("patch categoria: %d", code)
	}
	// Publicar registra o diretório público com a categoria já coerente.
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}

	eid := uuid.MustParse(eventID)
	var cat, dirCat string
	var catID uuid.UUID
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT e.category, e.category_id, d.category
			FROM events e JOIN public.event_directory d ON d.event_id = e.id
			WHERE e.id=$1`, eid).Scan(&cat, &catID, &dirCat); err != nil {
			t.Fatalf("ler categoria/diretório: %v", err)
		}
	})
	if cat != "teatro" || dirCat != "teatro" {
		t.Fatalf("categoria inconsistente: event=%s directory=%s (esperava teatro nos dois)", cat, dirCat)
	}
	// category_id aponta para a categoria 'teatro'.
	var slugOfID string
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT slug FROM event_categories WHERE id=$1`, catID).Scan(&slugOfID); err != nil {
			t.Fatalf("resolver category_id: %v", err)
		}
	})
	if slugOfID != "teatro" {
		t.Fatalf("category_id aponta para %s, esperava teatro", slugOfID)
	}

	// Evento publicado NÃO troca de categoria (regra de serviço) → 409.
	if code, _ := do(t, ts, "PATCH", "/api/v1/events/"+eventID, bearer(owner),
		map[string]any{"category": "festas"}); code != http.StatusConflict {
		t.Fatalf("trocar categoria de evento publicado: esperava 409, veio %d", code)
	}
}
