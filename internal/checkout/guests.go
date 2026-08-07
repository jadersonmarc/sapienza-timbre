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

// IssueCourtesy emite uma cortesia: registra o convidado e emite um ingresso ativo
// (transferível imediatamente). Com assento específico, ocupa-o para não ser vendido
// — se já estiver ocupado (hold ou ingresso), falha com inventory.ErrSeatUnavailable.
func IssueCourtesy(ctx context.Context, tx pgx.Tx, em Emitter, eventID uuid.UUID, lotID, seatID *uuid.UUID, name, cpf string) (uuid.UUID, error) {
	// Resolve o lote (cortesia precisa de um, para portaria/relatório).
	var lot uuid.UUID
	if lotID != nil {
		lot = *lotID
	} else if err := tx.QueryRow(ctx,
		`SELECT id FROM lots WHERE event_id=$1 ORDER BY position LIMIT 1`, eventID).Scan(&lot); err != nil {
		return uuid.Nil, fmt.Errorf("evento sem lote para cortesia: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO guest_list_entries (event_id, name, cpf, lot_id, seat_id, status)
		VALUES ($1,$2,$3,$4,$5,'issued')`,
		eventID, name, nilIfEmpty(cpf), lot, seatID); err != nil {
		return uuid.Nil, fmt.Errorf("registrar convidado: %w", err)
	}

	var ticketID uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO tickets (event_id, lot_id, seat_id, transferable_after, status, chain_status)
		VALUES ($1,$2,$3, now(), 'active','none') RETURNING id`,
		eventID, lot, seatID).Scan(&ticketID)
	if isUniqueViolation(err) {
		return uuid.Nil, inventory.ErrSeatUnavailable
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("emitir cortesia: %w", err)
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
