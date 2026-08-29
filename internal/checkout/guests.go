package checkout

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jadersonmarc/sapienza-timbre/internal/inventory"
)

// IssueCourtesy emite uma cortesia: registra o convidado (com categoria) e emite um
// ingresso ativo (transferível imediatamente). Com assento específico, ocupa-o para não
// ser vendido — se já estiver ocupado (hold ou ingresso), falha com ErrSeatUnavailable.
func IssueCourtesy(ctx context.Context, tx pgx.Tx, em Emitter, eventID uuid.UUID, lotID, seatID *uuid.UUID, categoryID uuid.UUID, name, cpf string) (uuid.UUID, error) {
	// Resolve o lote (cortesia precisa de um, para portaria/relatório).
	var lot uuid.UUID
	if lotID != nil {
		lot = *lotID
	} else if err := tx.QueryRow(ctx,
		`SELECT id FROM lots WHERE event_id=$1 ORDER BY sort_order LIMIT 1`, eventID).Scan(&lot); err != nil {
		return uuid.Nil, fmt.Errorf("evento sem lote para cortesia: %w", err)
	}

	var guestID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO guest_list_entries (event_id, name, cpf, lot_id, seat_id, courtesy_category_id, status)
		VALUES ($1,$2,$3,$4,$5,$6,'issued') RETURNING id`,
		eventID, name, nilIfEmpty(cpf), lot, seatID, categoryID).Scan(&guestID); err != nil {
		return uuid.Nil, fmt.Errorf("registrar convidado: %w", err)
	}

	var ticketID uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO tickets (event_id, lot_id, seat_id, transferable_after, status, chain_status)
		VALUES ($1,$2,$3, now(), 'active','not_materialized') RETURNING id`,
		eventID, lot, seatID).Scan(&ticketID)
	if isUniqueViolation(err) {
		return uuid.Nil, inventory.ErrSeatUnavailable
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("emitir cortesia: %w", err)
	}

	// Liga convidado e ingresso. Sem isso, quem lê o ingresso não chega na categoria — e a
	// categoria é o que a comprovação de público publica.
	if _, err := tx.Exec(ctx, `
		UPDATE guest_list_entries SET ticket_id=$2 WHERE id=$1`, guestID, ticketID); err != nil {
		return uuid.Nil, fmt.Errorf("ligar cortesia ao ingresso: %w", err)
	}

	if seatID != nil {
		if _, err := tx.Exec(ctx, `
			INSERT INTO seat_occupancy (event_id, seat_id, kind, ticket_id)
			VALUES ($1,$2,'ticket',$3)`, eventID, *seatID, ticketID); err != nil {
			if isUniqueViolation(err) {
				return uuid.Nil, inventory.ErrSeatUnavailable
			}
			return uuid.Nil, fmt.Errorf("ocupar assento da cortesia: %w", err)
		}
	}
	// Assina a cortesia (sem entrega — o convidado recebe por outro canal).
	if err := em.emit(ctx, tx, []uuid.UUID{ticketID}, ""); err != nil {
		return uuid.Nil, err
	}
	return ticketID, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
