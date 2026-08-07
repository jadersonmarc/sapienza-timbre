// Package checkout orquestra a compra: valida lote, reserva (hold de assentos ou
// estoque de pista), aplica cupom e meia-entrada, calcula o split, cria ordem/itens/
// pagamento e chama o gateway. O webhook (idempotente) confirma o pagamento, emite os
// ingressos e escreve o razão (ledger). Todas as funções recebem uma pgx.Tx já
// escopada por tenancy.WithTenant.
package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/catalog"
	"github.com/jadersonmarc/sapienza-timbre/internal/inventory"
	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
)

// Janela de contestação embutida em transferable_after: imediato em Pix, 60 dias em
// cartão (o ingresso nasce intransferível e só destrava ao fim dela).
const cardContestationWindow = 60 * 24 * time.Hour

var (
	// ErrLotUnavailable: lote inexistente ou não está à venda.
	ErrLotUnavailable = errors.New("checkout: lote indisponível")
	// ErrSeatsMismatch: quantidade de assentos difere da quantidade pedida.
	ErrSeatsMismatch = errors.New("checkout: informe um assento por ingresso")
	// ErrInsufficientStock: estoque insuficiente no lote (pista).
	ErrInsufficientStock = errors.New("checkout: estoque insuficiente")
	// ErrCoupon: cupom inválido, expirado ou esgotado.
	ErrCoupon = errors.New("checkout: cupom inválido")
	// ErrBadRequest: pedido malformado.
	ErrBadRequest = errors.New("checkout: pedido inválido")
)

// Producer carrega o que o checkout precisa do produtor (billing).
type Producer struct {
	ID            uuid.UUID
	RetentionPct  float64 // percentual retido pela plataforma sobre o total
	AsaasWalletID *string
}

// Request é o pedido de compra.
type Request struct {
	EventID      uuid.UUID   `json:"event_id"`
	LotID        uuid.UUID   `json:"lot_id"`
	Quantity     int         `json:"quantity"`
	SeatIDs      []uuid.UUID `json:"seat_ids"`       // se houver mapa; len == quantity
	HalfPriceQty int         `json:"half_price_qty"` // quantos são meia-entrada
	CouponCode   string      `json:"coupon_code"`
	Method       string      `json:"method"` // pix | credit_card
	Installments int         `json:"installments"`
	BuyerName    string      `json:"buyer_name"`
	BuyerEmail   string      `json:"buyer_email"`
	BuyerCPF     string      `json:"buyer_cpf"`
}

// Result é o retorno do StartCheckout.
type Result struct {
	OrderID     uuid.UUID `json:"order_id"`
	PaymentID   uuid.UUID `json:"payment_id"`
	AmountCents int64     `json:"amount_cents"`
	Method      string    `json:"method"`
	AsaasRef    string    `json:"asaas_ref"`
	PixCode     string    `json:"pix_code,omitempty"`
}

type splitInfo struct {
	ProducerCents  int64  `json:"producer_cents"`
	PlatformCents  int64  `json:"platform_cents"`
	ProducerWallet string `json:"producer_wallet,omitempty"`
}

// StartCheckout valida, reserva, precifica, cria ordem/pagamento e cria a cobrança no
// gateway. Deixa o pagamento 'pending'; a confirmação vem pelo webhook.
func StartCheckout(ctx context.Context, tx pgx.Tx, gw payment.PaymentGateway, prod Producer, req Request) (Result, error) {
	if req.Quantity <= 0 || (req.Method != payment.MethodPix && req.Method != payment.MethodCard) {
		return Result{}, ErrBadRequest
	}
	seated := len(req.SeatIDs) > 0
	if seated && len(req.SeatIDs) != req.Quantity {
		return Result{}, ErrSeatsMismatch
	}
	if req.HalfPriceQty < 0 || req.HalfPriceQty > req.Quantity {
		return Result{}, ErrBadRequest
	}

	lot, err := catalog.GetLot(ctx, tx, req.LotID)
	if err != nil {
		return Result{}, ErrLotUnavailable
	}
	if lot.Status != "active" || lot.EventID != req.EventID {
		return Result{}, ErrLotUnavailable
	}

	// Preço unitário por ingresso (assento usa sector_price_rules; pista usa o lote).
	unitPrices, err := unitPrices(ctx, tx, lot, req)
	if err != nil {
		return Result{}, err
	}
	subtotal := applyHalfPrice(unitPrices, req.HalfPriceQty)

	// Cupom (limite de uso e validade).
	couponID, discount, err := applyCoupon(ctx, tx, req.EventID, req.CouponCode, subtotal)
	if err != nil {
		return Result{}, err
	}
	total := max(subtotal-discount, 0)

	// Reserva.
	var holdID *uuid.UUID
	if seated {
		id, err := inventory.Hold(ctx, tx, req.EventID, req.SeatIDs, inventory.DefaultTTL)
		if err != nil {
			return Result{}, err
		}
		holdID = &id
	} else if lot.Sold+req.Quantity > lot.Stock {
		return Result{}, ErrInsufficientStock
	}

	// Split (taxa de conveniência retida pela plataforma vs. repasse ao produtor).
	split := computeSplit(total, prod)
	splitJSON, _ := json.Marshal(split)

	// Ordem.
	var orderID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO orders (event_id, buyer_email, buyer_cpf, coupon_id, total_cents, status)
		VALUES ($1,$2,$3,$4,$5,'pending') RETURNING id`,
		req.EventID, nilIfEmpty(req.BuyerEmail), nilIfEmpty(req.BuyerCPF), couponID, total,
	).Scan(&orderID); err != nil {
		return Result{}, fmt.Errorf("criar ordem: %w", err)
	}

	// Itens: um por assento (seated) ou um agregado (pista).
	if err := insertOrderItems(ctx, tx, orderID, lot, req, unitPrices); err != nil {
		return Result{}, err
	}

	// Liga o hold à ordem (o webhook acha o hold por order_id).
	if holdID != nil {
		if _, err := tx.Exec(ctx, `UPDATE holds SET order_id=$1 WHERE id=$2`, orderID, *holdID); err != nil {
			return Result{}, fmt.Errorf("vincular hold à ordem: %w", err)
		}
	}

	// Pagamento (pending).
	var paymentID uuid.UUID
	installments := max(req.Installments, 1)
	if err := tx.QueryRow(ctx, `
		INSERT INTO payments (order_id, method, installments, amount_cents, status, split)
		VALUES ($1,$2,$3,$4,'pending',$5::jsonb) RETURNING id`,
		orderID, req.Method, installments, total, string(splitJSON),
	).Scan(&paymentID); err != nil {
		return Result{}, fmt.Errorf("criar pagamento: %w", err)
	}

	// Cobrança no gateway.
	var splitItems []payment.SplitItem
	if prod.AsaasWalletID != nil && *prod.AsaasWalletID != "" && split.ProducerCents > 0 {
		splitItems = []payment.SplitItem{{WalletID: *prod.AsaasWalletID, FixedCents: split.ProducerCents}}
	}
	charge, err := gw.CreateCharge(ctx, payment.ChargeRequest{
		OrderID: orderID.String(), Method: req.Method, AmountCents: total, Installments: installments,
		BuyerName: req.BuyerName, BuyerEmail: req.BuyerEmail, BuyerCPF: req.BuyerCPF, Split: splitItems,
	})
	if err != nil {
		return Result{}, fmt.Errorf("criar cobrança: %w", err)
	}

	// Grava a referência e o índice público (webhook global → tenant).
	if _, err := tx.Exec(ctx, `UPDATE payments SET asaas_ref=$1 WHERE id=$2`, charge.AsaasRef, paymentID); err != nil {
		return Result{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO public.payment_index (asaas_ref, producer_id, order_id)
		VALUES ($1,$2,$3) ON CONFLICT (asaas_ref) DO NOTHING`,
		charge.AsaasRef, prod.ID, orderID); err != nil {
		return Result{}, fmt.Errorf("indexar pagamento: %w", err)
	}

	return Result{
		OrderID: orderID, PaymentID: paymentID, AmountCents: total,
		Method: req.Method, AsaasRef: charge.AsaasRef, PixCode: charge.PixCode,
	}, nil
}

// ConfirmPayment confirma um pagamento (idempotente): emite ingressos, marca ordem
// paga e escreve o razão. Chamado pelo webhook após resolver o tenant. Devolve os
// ingressos emitidos (vazio se já estava confirmado).
func ConfirmPayment(ctx context.Context, tx pgx.Tx, em Emitter, asaasRef string) ([]uuid.UUID, error) {
	var paymentID, orderID uuid.UUID
	var method, status string
	var amount int64
	var splitRaw []byte
	err := tx.QueryRow(ctx, `
		SELECT id, order_id, method, status, amount_cents, split
		  FROM payments WHERE asaas_ref = $1 FOR UPDATE`, asaasRef).
		Scan(&paymentID, &orderID, &method, &status, &amount, &splitRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // pagamento desconhecido: ignora (webhook de outro sistema)
	}
	if err != nil {
		return nil, err
	}
	if status == "confirmed" {
		return nil, nil // idempotente
	}

	// Dados da ordem e do item (lote/quantidade).
	var eventID uuid.UUID
	var couponID *uuid.UUID
	var buyerEmail *string
	if err := tx.QueryRow(ctx, `SELECT event_id, coupon_id, buyer_email FROM orders WHERE id=$1`, orderID).Scan(&eventID, &couponID, &buyerEmail); err != nil {
		return nil, err
	}
	var lotID uuid.UUID
	var quantity int
	if err := tx.QueryRow(ctx, `SELECT lot_id, COALESCE(SUM(quantity),0) FROM order_items WHERE order_id=$1 GROUP BY lot_id LIMIT 1`, orderID).Scan(&lotID, &quantity); err != nil {
		return nil, err
	}

	transferableAfter := time.Now()
	if method == payment.MethodCard {
		transferableAfter = transferableAfter.Add(cardContestationWindow)
	}

	// Emite os ingressos.
	var tickets []uuid.UUID
	var holdID uuid.UUID
	holdErr := tx.QueryRow(ctx, `SELECT id FROM holds WHERE order_id=$1 AND status='held'`, orderID).Scan(&holdID)
	switch {
	case holdErr == nil: // com mapa: converte o hold em ingressos
		tickets, err = inventory.Confirm(ctx, tx, holdID, orderID, lotID, transferableAfter)
		if err != nil {
			return nil, err
		}
	case errors.Is(holdErr, pgx.ErrNoRows): // pista: debita estoque e emite N ingressos
		if _, err := catalog.SellFromLot(ctx, tx, lotID, quantity); err != nil {
			return nil, err
		}
		for range quantity {
			var tid uuid.UUID
			if err := tx.QueryRow(ctx, `
				INSERT INTO tickets (event_id, lot_id, order_id, transferable_after, status, chain_status)
				VALUES ($1,$2,$3,$4,'active','none') RETURNING id`,
				eventID, lotID, orderID, transferableAfter).Scan(&tid); err != nil {
				return nil, fmt.Errorf("emitir ingresso: %w", err)
			}
			tickets = append(tickets, tid)
		}
	default:
		return nil, holdErr
	}

	// Confirma pagamento e ordem; consome o cupom.
	if _, err := tx.Exec(ctx, `UPDATE payments SET status='confirmed', settled_at=now() WHERE id=$1`, paymentID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET status='paid', updated_at=now() WHERE id=$1`, orderID); err != nil {
		return nil, err
	}
	if couponID != nil {
		if _, err := tx.Exec(ctx, `UPDATE coupons SET uses = uses + 1 WHERE id=$1`, *couponID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO coupon_redemptions (coupon_id, order_id) VALUES ($1,$2)`, *couponID, orderID); err != nil {
			return nil, err
		}
	}

	// Assina os ingressos (Ed25519) e entrega o QR ao comprador.
	deliverTo := ""
	if buyerEmail != nil {
		deliverTo = *buyerEmail
	}
	if err := em.emit(ctx, tx, tickets, deliverTo); err != nil {
		return nil, err
	}

	// Razão: taxa (plataforma), repasse (produtor, D+2 após o evento) e, no cartão,
	// retenção de 5% por 60 dias como reserva de contestação.
	if err := writeLedger(ctx, tx, eventID, orderID, paymentID, method, splitRaw); err != nil {
		return nil, err
	}
	return tickets, nil
}
