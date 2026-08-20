package checkout

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
)

// writeTicketDirectory alimenta o índice público de ingressos do comprador (public.
// ticket_directory) — o que "meus ingressos" lê sem varrer schemas de produtor. Guarda um
// snapshot do evento (para listar sem tocar o tenant) e o token do QR. O subject_id vem da
// compra autenticada; o vínculo por e-mail verificado cobre eventuais ingressos legados
// (compra antiga como convidado). Idempotente (webhook re-executa).
//
// É um dos PONTOS DE ESCRITA do ticket_directory (§3.10). Os outros: estorno/queima (marca
// status em refund.go) e, na Onda 2, transferência/revenda (reatribui subject_id).
func writeTicketDirectory(ctx context.Context, tx pgx.Tx, producerID, eventID uuid.UUID, buyerEmail string, ticketIDs []uuid.UUID) error {
	if len(ticketIDs) == 0 {
		return nil
	}
	var title string
	var startsAt *time.Time
	var city *string
	if err := tx.QueryRow(ctx, `SELECT title, starts_at, city FROM events WHERE id=$1`, eventID).
		Scan(&title, &startsAt, &city); err != nil {
		return err
	}
	// subject já existente com esse e-mail (senão nulo — o OTP vincula depois).
	var subjectID *uuid.UUID
	if buyerEmail != "" {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM subjects WHERE lower(email)=lower($1) ORDER BY created_at LIMIT 1`, buyerEmail).Scan(&id); err == nil {
			subjectID = &id
		}
	}
	for _, tid := range ticketIDs {
		token, err := ticketing.TicketToken(ctx, tx, tid)
		if err != nil {
			return err
		}
		var seatLabel *string
		var chainStatus string
		_ = tx.QueryRow(ctx, `
			SELECT COALESCE(s.row_label,'') || COALESCE(s.number,'')
			  FROM tickets t JOIN seats s ON s.id = t.seat_id WHERE t.id = $1`, tid).Scan(&seatLabel)
		if err := tx.QueryRow(ctx, `SELECT chain_status FROM tickets WHERE id=$1`, tid).Scan(&chainStatus); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO ticket_directory
			    (subject_id, buyer_email, producer_id, event_id, event_title, event_starts_at, venue_city, ticket_id, token, seat_label, status, chain_status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'active',$11)
			ON CONFLICT (producer_id, ticket_id) DO NOTHING`,
			subjectID, nilIfEmpty(buyerEmail), producerID, eventID, title, startsAt, city, tid, token, seatLabel, chainStatus); err != nil {
			return err
		}
	}
	return nil
}
