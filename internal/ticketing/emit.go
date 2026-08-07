package ticketing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrUnsigned: o ingresso ainda não tem assinatura/QR.
var ErrUnsigned = errors.New("ticketing: ingresso não assinado")

// row carrega os campos do ingresso que compõem o payload (determinístico).
type row struct {
	eventID, lotID uuid.UUID
	seatID         *uuid.UUID
	createdAt      time.Time
	face           int64
}

func loadRow(ctx context.Context, tx pgx.Tx, ticketID uuid.UUID) (row, error) {
	var r row
	err := tx.QueryRow(ctx, `
		SELECT t.event_id, t.lot_id, t.seat_id, t.created_at, l.price_cents
		  FROM tickets t JOIN lots l ON l.id = t.lot_id
		 WHERE t.id = $1`, ticketID).Scan(&r.eventID, &r.lotID, &r.seatID, &r.createdAt, &r.face)
	return r, err
}

func payloadOf(ticketID uuid.UUID, r row, nonce string) Payload {
	return Payload{
		TicketID: ticketID, EventID: r.eventID, LotID: r.lotID, SeatID: r.seatID,
		FaceCents: r.face, Nonce: nonce, IssuedAt: r.createdAt.Unix(),
	}
}

// SignTicket assina o ingresso e grava signature + qr_nonce. Determinístico: o token
// pode ser reconstruído depois a partir da linha (TicketToken).
func SignTicket(ctx context.Context, tx pgx.Tx, s *Signer, ticketID uuid.UUID) error {
	r, err := loadRow(ctx, tx, ticketID)
	if err != nil {
		return fmt.Errorf("carregar ingresso: %w", err)
	}
	nonce := NewNonce()
	sig, err := s.Sign(payloadOf(ticketID, r, nonce))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE tickets SET signature = $2, qr_nonce = $3, updated_at = now() WHERE id = $1`,
		ticketID, sig, nonce); err != nil {
		return fmt.Errorf("gravar assinatura: %w", err)
	}
	return nil
}

// TicketToken reconstrói o token do QR a partir da linha assinada.
func TicketToken(ctx context.Context, tx pgx.Tx, ticketID uuid.UUID) (string, error) {
	var r row
	var nonce *string
	var sig []byte
	err := tx.QueryRow(ctx, `
		SELECT t.event_id, t.lot_id, t.seat_id, t.created_at, l.price_cents, t.qr_nonce, t.signature
		  FROM tickets t JOIN lots l ON l.id = t.lot_id
		 WHERE t.id = $1`, ticketID).Scan(&r.eventID, &r.lotID, &r.seatID, &r.createdAt, &r.face, &nonce, &sig)
	if err != nil {
		return "", err
	}
	if nonce == nil || len(sig) == 0 {
		return "", ErrUnsigned
	}
	msg, err := json.Marshal(payloadOf(ticketID, r, *nonce))
	if err != nil {
		return "", err
	}
	return b64.EncodeToString(msg) + "." + b64.EncodeToString(sig), nil
}
