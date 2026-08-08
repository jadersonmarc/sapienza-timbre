// Package season é o passe de temporada (Etapa 2.3): a compra de um passe emite UM
// ingresso por data. Cada ingresso é um ticket normal — destacável e repassável
// individualmente (reusa transferência restrita e mercado secundário). Roda sob
// tenancy.WithTenant.
package season

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/checkout"
	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
	"github.com/jadersonmarc/sapienza-timbre/internal/program"
)

// ErrNoDates: passe sem datas não pode ser comprado.
var ErrNoDates = errors.New("season: passe sem datas")

// Pass é um passe de temporada.
type Pass struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	PriceCents int64     `json:"price_cents"`
}

// Date é uma data do passe.
type Date struct {
	ID           uuid.UUID  `json:"id"`
	SeasonPassID uuid.UUID  `json:"season_pass_id"`
	EventID      *uuid.UUID `json:"event_id,omitempty"`
	LotID        *uuid.UUID `json:"lot_id,omitempty"`
	OccursAt     *time.Time `json:"occurs_at,omitempty"`
	Detachable   bool       `json:"detachable"`
	Transferable bool       `json:"transferable"`
}

// CreatePass cria um passe de temporada.
func CreatePass(ctx context.Context, tx pgx.Tx, name string, priceCents int64) (Pass, error) {
	var p Pass
	err := tx.QueryRow(ctx, `
		INSERT INTO season_passes (name, price_cents) VALUES ($1,$2)
		RETURNING id, name, price_cents`, name, priceCents).Scan(&p.ID, &p.Name, &p.PriceCents)
	return p, err
}

// AddDate adiciona uma data ao passe (evento + lote que o ingresso ocupará).
func AddDate(ctx context.Context, tx pgx.Tx, passID, eventID, lotID uuid.UUID, occursAt *time.Time, detachable, transferable bool) (Date, error) {
	var d Date
	err := tx.QueryRow(ctx, `
		INSERT INTO season_pass_dates (season_pass_id, event_id, lot_id, occurs_at, detachable, transferable)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id, season_pass_id, event_id, lot_id, occurs_at, detachable, transferable`,
		passID, eventID, lotID, occursAt, detachable, transferable).
		Scan(&d.ID, &d.SeasonPassID, &d.EventID, &d.LotID, &d.OccursAt, &d.Detachable, &d.Transferable)
	return d, err
}

// BuyResult é o retorno da compra do passe.
type BuyResult struct {
	OrderID    uuid.UUID `json:"order_id"`
	PriceCents int64     `json:"price_cents"`
	AsaasRef   string    `json:"asaas_ref"`
	PixCode    string    `json:"pix_code,omitempty"`
}

// BuyPass cria a ordem e a cobrança do passe. Os ingressos só são emitidos na
// confirmação (ConfirmPass).
func BuyPass(ctx context.Context, tx pgx.Tx, gw payment.PaymentGateway, producerID, passID uuid.UUID, buyerEmail string) (BuyResult, error) {
	var price int64
	if err := tx.QueryRow(ctx, `SELECT price_cents FROM season_passes WHERE id=$1`, passID).Scan(&price); err != nil {
		return BuyResult{}, err
	}
	// Evento de referência da ordem = o da primeira data.
	var firstEvent uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT event_id FROM season_pass_dates WHERE season_pass_id=$1 AND event_id IS NOT NULL ORDER BY occurs_at NULLS LAST, id LIMIT 1`, passID).Scan(&firstEvent); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BuyResult{}, ErrNoDates
		}
		return BuyResult{}, err
	}

	var orderID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO orders (event_id, buyer_email, total_cents, status, season_pass_id)
		VALUES ($1,$2,$3,'pending',$4) RETURNING id`, firstEvent, nilStr(buyerEmail), price, passID).Scan(&orderID); err != nil {
		return BuyResult{}, err
	}
	var paymentID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO payments (order_id, method, amount_cents, status)
		VALUES ($1,'pix',$2,'pending') RETURNING id`, orderID, price).Scan(&paymentID); err != nil {
		return BuyResult{}, err
	}
	charge, err := gw.CreateCharge(ctx, payment.ChargeRequest{
		OrderID: orderID.String(), Method: payment.MethodPix, AmountCents: price, BuyerEmail: buyerEmail,
	})
	if err != nil {
		return BuyResult{}, fmt.Errorf("cobrança: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE payments SET asaas_ref=$1 WHERE id=$2`, charge.AsaasRef, paymentID); err != nil {
		return BuyResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO public.payment_index (asaas_ref, producer_id, order_id, kind)
		VALUES ($1,$2,$3,'season') ON CONFLICT (asaas_ref) DO NOTHING`, charge.AsaasRef, producerID, orderID); err != nil {
		return BuyResult{}, err
	}
	return BuyResult{OrderID: orderID, PriceCents: price, AsaasRef: charge.AsaasRef, PixCode: charge.PixCode}, nil
}

// ConfirmPass emite um ingresso por data do passe (assinado/entregue via Emitter) e
// fecha o financeiro. Idempotente.
func ConfirmPass(ctx context.Context, tx pgx.Tx, em checkout.Emitter, producerID uuid.UUID, asaasRef string) error {
	var paymentID, orderID uuid.UUID
	var method, status string
	err := tx.QueryRow(ctx, `SELECT id, order_id, method, status FROM payments WHERE asaas_ref=$1 FOR UPDATE`, asaasRef).Scan(&paymentID, &orderID, &method, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status == "confirmed" {
		return nil
	}
	var passID uuid.UUID
	var buyerEmail *string
	if err := tx.QueryRow(ctx, `SELECT season_pass_id, buyer_email FROM orders WHERE id=$1`, orderID).Scan(&passID, &buyerEmail); err != nil {
		return err
	}

	transferableAfter := time.Now()
	if method == payment.MethodCard {
		transferableAfter = transferableAfter.Add(60 * 24 * time.Hour)
	}

	rows, err := tx.Query(ctx, `SELECT event_id, lot_id FROM season_pass_dates WHERE season_pass_id=$1 AND lot_id IS NOT NULL ORDER BY occurs_at NULLS LAST, id`, passID)
	if err != nil {
		return err
	}
	type dt struct{ eventID, lotID uuid.UUID }
	var dates []dt
	for rows.Next() {
		var d dt
		if err := rows.Scan(&d.eventID, &d.lotID); err != nil {
			rows.Close()
			return err
		}
		dates = append(dates, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var tickets []uuid.UUID
	for _, d := range dates {
		var tid uuid.UUID
		if err := tx.QueryRow(ctx, `
			INSERT INTO tickets (event_id, lot_id, order_id, season_pass_id, transferable_after, status, chain_status)
			VALUES ($1,$2,$3,$4,$5,'active','none') RETURNING id`,
			d.eventID, d.lotID, orderID, passID, transferableAfter).Scan(&tid); err != nil {
			return fmt.Errorf("emitir ingresso do passe: %w", err)
		}
		tickets = append(tickets, tid)
	}

	deliverTo := ""
	if buyerEmail != nil {
		deliverTo = *buyerEmail
	}
	if err := em.EmitTickets(ctx, tx, tickets, deliverTo); err != nil {
		return err
	}

	// Apuração e razão do passe (taxa 15% − rebate do nível na data da venda, repasse,
	// originação) — centralizado em program.SettleLedger.
	if err := program.SettleLedger(ctx, tx, producerID, orderID, paymentID); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `UPDATE payments SET status='confirmed', settled_at=now() WHERE id=$1`, paymentID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE orders SET status='paid', updated_at=now() WHERE id=$1`, orderID)
	return err
}

func nilStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
