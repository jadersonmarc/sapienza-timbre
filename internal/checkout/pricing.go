package checkout

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/catalog"
)

// unitPrices devolve o preço de cada ingresso, na ordem do pedido. Com mapa, usa
// sector_price_rules (lote × setor do assento), caindo no preço do lote quando não há
// regra. Em pista, são `quantity` cópias do preço do lote.
func unitPrices(ctx context.Context, tx pgx.Tx, lot catalog.Lot, req Request) ([]int64, error) {
	if len(req.SeatIDs) == 0 {
		out := make([]int64, req.Quantity)
		for i := range out {
			out[i] = lot.PriceCents
		}
		return out, nil
	}
	ids := make([]string, len(req.SeatIDs))
	for i, s := range req.SeatIDs {
		ids[i] = s.String()
	}
	rows, err := tx.Query(ctx, `
		SELECT s.id, COALESCE(spr.price_cents, $2)
		  FROM seats s
		  LEFT JOIN sector_price_rules spr ON spr.sector_id = s.sector_id AND spr.lot_id = $3
		 WHERE s.id = ANY($1::uuid[])`, ids, lot.PriceCents, lot.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	priceOf := make(map[uuid.UUID]int64, len(req.SeatIDs))
	for rows.Next() {
		var id uuid.UUID
		var price int64
		if err := rows.Scan(&id, &price); err != nil {
			return nil, err
		}
		priceOf[id] = price
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]int64, len(req.SeatIDs))
	for i, s := range req.SeatIDs {
		p, ok := priceOf[s]
		if !ok {
			return nil, ErrBadRequest // assento inexistente
		}
		out[i] = p
	}
	return out, nil
}

// applyHalfPrice soma os preços, cobrando metade nos `halfQty` primeiros ingressos
// (meia-entrada = 50%).
func applyHalfPrice(prices []int64, halfQty int) int64 {
	var total int64
	for i, p := range prices {
		if i < halfQty {
			total += p / 2
		} else {
			total += p
		}
	}
	return total
}

// applyCoupon valida o cupom (janela e limite de uso) e devolve o desconto.
func applyCoupon(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, code string, subtotal int64) (*uuid.UUID, int64, error) {
	if code == "" {
		return nil, 0, nil
	}
	var id uuid.UUID
	var pct *float64
	var cents, maxUses *int64
	var uses int64
	var from, until *time.Time
	err := tx.QueryRow(ctx, `
		SELECT id, discount_pct, discount_cents, max_uses, uses, valid_from, valid_until
		  FROM coupons WHERE event_id = $1 AND code = $2`, eventID, code).
		Scan(&id, &pct, &cents, &maxUses, &uses, &from, &until)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, ErrCoupon
	}
	if err != nil {
		return nil, 0, err
	}
	now := time.Now()
	if (from != nil && now.Before(*from)) || (until != nil && now.After(*until)) {
		return nil, 0, ErrCoupon
	}
	if maxUses != nil && uses >= *maxUses {
		return nil, 0, ErrCoupon
	}
	var discount int64
	switch {
	case pct != nil:
		discount = int64(math.Round(float64(subtotal) * *pct / 100))
	case cents != nil:
		discount = *cents
	}
	discount = min(discount, subtotal)
	return &id, discount, nil
}

// insertOrderItems grava os itens: um por assento (com mapa) ou um agregado (pista).
func insertOrderItems(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, lot catalog.Lot, req Request, prices []int64) error {
	if len(req.SeatIDs) == 0 {
		_, err := tx.Exec(ctx, `
			INSERT INTO order_items (order_id, lot_id, quantity, unit_price_cents, half_price)
			VALUES ($1,$2,$3,$4,$5)`,
			orderID, lot.ID, req.Quantity, lot.PriceCents, req.HalfPriceQty > 0)
		return err
	}
	for i, seat := range req.SeatIDs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO order_items (order_id, lot_id, seat_id, quantity, unit_price_cents, half_price)
			VALUES ($1,$2,$3,1,$4,$5)`,
			orderID, lot.ID, seat, prices[i], i < req.HalfPriceQty); err != nil {
			return err
		}
	}
	return nil
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
