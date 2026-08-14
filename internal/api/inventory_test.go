package api_test

import (
	"context"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-timbre/internal/catalog"
	"github.com/jadersonmarc/sapienza-timbre/internal/inventory"
)

// seatedEvent cria um evento com um setor e `seatCount` assentos (fileira A) + um
// lote, e devolve os ids. Os assentos vêm ordenados (A1, A2, ...).
func seatedEvent(t *testing.T, ts *httptest.Server, pool *pgxpool.Pool, owner string, pid uuid.UUID, seatCount int) (eventID uuid.UUID, seats []uuid.UUID, lotID uuid.UUID) {
	t.Helper()
	eventID = uuid.MustParse(createEvent(t, ts, owner, "Peça", "teatro"))
	ctx := context.Background()
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		sec, err := catalog.CreateSector(ctx, tx, catalog.Sector{EventID: eventID, Name: "Plateia", Kind: "seated"})
		if err != nil {
			t.Fatalf("criar setor: %v", err)
		}
		if _, err := catalog.GenerateSeats(ctx, tx, sec.ID, 1, seatCount, "alpha"); err != nil {
			t.Fatalf("gerar assentos: %v", err)
		}
		list, err := catalog.ListSeats(ctx, tx, sec.ID)
		if err != nil {
			t.Fatalf("listar assentos: %v", err)
		}
		for _, s := range list {
			seats = append(seats, s.ID)
		}
		lot, err := catalog.CreateLot(ctx, tx, catalog.Lot{EventID: eventID, Name: "Lote 1", PriceCents: 5000, Quantity: 100})
		if err != nil {
			t.Fatalf("criar lote: %v", err)
		}
		lotID = lot.ID
	})
	return eventID, seats, lotID
}

// holdTx roda inventory.Hold numa transação própria e faz commit no sucesso.
func holdTx(ctx context.Context, pool *pgxpool.Pool, pid, eventID uuid.UUID, seatIDs []uuid.UUID, ttl time.Duration) (uuid.UUID, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, pid); err != nil {
		return uuid.Nil, err
	}
	holdID, err := inventory.Hold(ctx, tx, eventID, seatIDs, ttl)
	if err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return holdID, nil
}

// TestReservationOneWinner é o "pronto quando" da Etapa 1.3: N goroutines disputando
// o mesmo assento produzem EXATAMENTE um vencedor. A garantia é do índice único
// parcial de seat_occupancy, não da aplicação.
func TestReservationOneWinner(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Conc", "owner@conc.com", "senha1234")
	pid := producerID(t, ts, owner)
	eventID, seats, _ := seatedEvent(t, ts, pool, owner, pid, 1)
	seat := seats[0]
	ctx := context.Background()

	const n = 24
	var wins int64
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := holdTx(ctx, pool, pid, eventID, []uuid.UUID{seat}, time.Minute); err == nil {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("esperava exatamente 1 vencedor, veio %d", wins)
	}
	// Exatamente uma ocupação viva para o assento.
	var live int
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM seat_occupancy WHERE seat_id=$1 AND NOT released`, seat).Scan(&live); err != nil {
			t.Fatalf("contar ocupação: %v", err)
		}
	})
	if live != 1 {
		t.Fatalf("esperava 1 ocupação viva, veio %d", live)
	}
}

// TestHoldReleaseConfirm cobre o ciclo de vida e a exclusão hold×ticket.
func TestHoldReleaseConfirm(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Ciclo", "owner@ciclo.com", "senha1234")
	pid := producerID(t, ts, owner)
	eventID, seats, lotID := seatedEvent(t, ts, pool, owner, pid, 2)
	ctx := context.Background()

	// Hold dos 2 assentos.
	holdID, err := holdTx(ctx, pool, pid, eventID, seats, time.Minute)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	// Re-hold dos mesmos assentos falha (ocupados).
	if _, err := holdTx(ctx, pool, pid, eventID, seats, time.Minute); err != inventory.ErrSeatUnavailable {
		t.Fatalf("re-hold: esperava ErrSeatUnavailable, veio %v", err)
	}
	// Release libera.
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := inventory.Release(ctx, tx, holdID); err != nil {
			t.Fatalf("release: %v", err)
		}
	})
	// Agora dá pra segurar de novo.
	holdID2, err := holdTx(ctx, pool, pid, eventID, seats, time.Minute)
	if err != nil {
		t.Fatalf("hold pós-release: %v", err)
	}
	// Confirm converte o hold em ingressos na mesma transação. O checkout (1.4) cria
	// a ordem antes; aqui criamos uma mínima para satisfazer a FK de tickets.order_id.
	var tickets []uuid.UUID
	var orderID uuid.UUID
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `INSERT INTO orders (event_id) VALUES ($1) RETURNING id`, eventID).Scan(&orderID); err != nil {
			t.Fatalf("criar ordem: %v", err)
		}
		var e error
		tickets, e = inventory.Confirm(ctx, tx, holdID2, orderID, lotID, time.Now())
		if e != nil {
			t.Fatalf("confirm: %v", e)
		}
	})
	if len(tickets) != 2 {
		t.Fatalf("esperava 2 ingressos, veio %d", len(tickets))
	}
	// Assento ocupado por INGRESSO: novo hold falha (prova hold×ticket).
	if _, err := holdTx(ctx, pool, pid, eventID, seats[:1], time.Minute); err != inventory.ErrSeatUnavailable {
		t.Fatalf("hold sobre assento com ingresso: esperava ErrSeatUnavailable, veio %v", err)
	}
	// Os ingressos nascem ativos.
	var active int
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tickets WHERE order_id=$1 AND status='active'`, orderID).Scan(&active); err != nil {
			t.Fatalf("contar ingressos: %v", err)
		}
	})
	if active != 2 {
		t.Fatalf("esperava 2 ingressos ativos, veio %d", active)
	}
}

// TestHoldExpirySweep: a varredura libera holds vencidos e o assento volta a ficar
// disponível.
func TestHoldExpirySweep(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Exp", "owner@exp.com", "senha1234")
	pid := producerID(t, ts, owner)
	eventID, seats, _ := seatedEvent(t, ts, pool, owner, pid, 1)
	ctx := context.Background()

	// TTL que arredonda para 0s → expira imediatamente.
	if _, err := holdTx(ctx, pool, pid, eventID, seats, time.Millisecond); err != nil {
		t.Fatalf("hold: %v", err)
	}
	// A varredura libera a ocupação vencida.
	var released int64
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		var e error
		released, e = inventory.ExpireDue(ctx, tx)
		if e != nil {
			t.Fatalf("expire: %v", e)
		}
	})
	if released < 1 {
		t.Fatalf("esperava ao menos 1 ocupação liberada, veio %d", released)
	}
	// Assento livre de novo.
	if _, err := holdTx(ctx, pool, pid, eventID, seats, time.Minute); err != nil {
		t.Fatalf("hold pós-expiração: %v", err)
	}
}

// TestAntiHole: com a regra ligada, uma reserva que deixaria um assento isolado é
// recusada.
func TestAntiHole(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Buraco", "owner@buraco.com", "senha1234")
	pid := producerID(t, ts, owner)
	eventID, seats, _ := seatedEvent(t, ts, pool, owner, pid, 3) // A1, A2, A3
	ctx := context.Background()

	// Liga a regra anti-buraco no evento.
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `UPDATE events SET anti_hole=true WHERE id=$1`, eventID); err != nil {
			t.Fatalf("ligar anti_hole: %v", err)
		}
	})

	// Segurar A1 e A3 deixaria A2 isolado → recusa.
	if _, err := holdTx(ctx, pool, pid, eventID, []uuid.UUID{seats[0], seats[2]}, time.Minute); err != inventory.ErrAntiHole {
		t.Fatalf("anti-buraco: esperava ErrAntiHole, veio %v", err)
	}
	// Segurar os três não deixa buraco → ok.
	if _, err := holdTx(ctx, pool, pid, eventID, seats, time.Minute); err != nil {
		t.Fatalf("hold dos três: %v", err)
	}
}
