package checkout

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/catalog"
)

// RefundPayment processa uma contestação/estorno (idempotente): QUEIMA os ingressos
// ativos da ordem (antes que circulem), libera os assentos, marca pagamento e ordem
// como estornados e lança o estorno no razão. É o que a "contestação dentro da janela"
// dispara — o ingresso é invalidado antes de poder ser transferido.
func RefundPayment(ctx context.Context, tx pgx.Tx, asaasRef string) error {
	var orderID uuid.UUID
	var eventID uuid.UUID
	var status string
	err := tx.QueryRow(ctx, `
		SELECT p.order_id, o.event_id, p.status
		  FROM payments p JOIN orders o ON o.id = p.order_id
		 WHERE p.asaas_ref = $1 FOR UPDATE OF p`, asaasRef).Scan(&orderID, &eventID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // desconhecido: ignora
	}
	if err != nil {
		return err
	}
	if status == "refunded" {
		return nil // idempotente
	}

	// Devolve capacidade ao(s) lote(s) ANTES de mexer nos assentos (ordem lote→assento).
	// Conta os ingressos ativos por lote e decrementa sold_count com piso em 0 (Correção
	// 4.2): estorno/queima duplicados nunca levam o contador a negativo.
	byLot, err := tx.Query(ctx, `
		SELECT lot_id, count(*) FROM tickets
		 WHERE order_id = $1 AND status = 'active' GROUP BY lot_id`, orderID)
	if err != nil {
		return err
	}
	type lotQty struct {
		lot uuid.UUID
		qty int
	}
	var refunds []lotQty
	for byLot.Next() {
		var lq lotQty
		if err := byLot.Scan(&lq.lot, &lq.qty); err != nil {
			byLot.Close()
			return err
		}
		refunds = append(refunds, lq)
	}
	byLot.Close()
	if err := byLot.Err(); err != nil {
		return err
	}
	for _, lq := range refunds {
		if err := catalog.RefundFromLot(ctx, tx, lq.lot, lq.qty); err != nil {
			return err
		}
	}

	// Libera os assentos dos ingressos ativos da ordem e queima os ingressos.
	if _, err := tx.Exec(ctx, `
		UPDATE seat_occupancy SET released = true
		 WHERE ticket_id IN (SELECT id FROM tickets WHERE order_id = $1 AND status = 'active')`, orderID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tickets SET status = 'burned', updated_at = now()
		 WHERE order_id = $1 AND status = 'active'`, orderID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE payments SET status='refunded', updated_at=now() WHERE asaas_ref=$1`, asaasRef); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET status='refunded', updated_at=now() WHERE id=$1`, orderID); err != nil {
		return err
	}
	// Estorno no razão (valor negativo).
	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries (event_id, order_id, kind, amount_cents, available_at)
		SELECT $2, id, 'estorno', -total_cents, now() FROM orders WHERE id = $1`, orderID, eventID); err != nil {
		return err
	}
	return nil
}
