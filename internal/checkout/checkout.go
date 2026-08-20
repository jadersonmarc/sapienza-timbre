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
	"github.com/jadersonmarc/sapienza-timbre/internal/pricing"
	"github.com/jadersonmarc/sapienza-timbre/internal/program"
	"github.com/jadersonmarc/sapienza-timbre/internal/promo"
)

// Janela de contestação embutida em transferable_after: imediato em Pix, 60 dias em
// cartão (o ingresso nasce intransferível e só destrava ao fim dela).
const cardContestationWindow = 60 * 24 * time.Hour

// maxLotResolveAttempts limita o retry de resolução do lote corrente quando o lote vira
// (por esgotamento) entre resolver e reservar. Constante isolada — valor PROVISÓRIO
// sugerido (Correção 2). O laço converge por "mesmo lote duas vezes" (esgotamento
// genuíno) ou por este limite.
const maxLotResolveAttempts = 3

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
	CampaignID   *uuid.UUID  `json:"campaign_id"` // atribuição de origem (UTM)
	Method       string      `json:"method"`      // pix | credit_card
	Installments int         `json:"installments"`
	BuyerName    string      `json:"buyer_name"`
	BuyerEmail   string      `json:"buyer_email"`
	BuyerCPF     string      `json:"buyer_cpf"`
	// SubjectID identifica o comprador autenticado (preenchido pelo handler a partir do
	// token de comprador; nunca vem do corpo).
	SubjectID uuid.UUID `json:"-"`
}

// Result é o retorno do StartCheckout. AmountCents é o TOTAL cobrado (face + conveniência);
// FaceCents e ConvenienceFeeCents dão a decomposição para a tela de confirmação.
type Result struct {
	OrderID             uuid.UUID `json:"order_id"`
	PaymentID           uuid.UUID `json:"payment_id"`
	AmountCents         int64     `json:"amount_cents"`
	FaceCents           int64     `json:"face_cents"`
	ConvenienceFeeCents int64     `json:"convenience_fee_cents"`
	Method              string    `json:"method"`
	AsaasRef            string    `json:"asaas_ref"`
	PixCode             string    `json:"pix_code,omitempty"`
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

	// TRAVA DE ORDEM (não negociar): sempre LOTE primeiro (ReserveFromLot = UPDATE lots,
	// trava a linha do lote), ASSENTO depois (inventory.Hold trava seat_occupancy). Ordem
	// invertida entre caminhos = deadlock sob concorrência. Vale para pista E seated.
	//
	// Resolve o lote VIGENTE e reserva nele (held_count), com retry na virada (Correção 2):
	// o lote pode esgotar entre resolver e reservar; re-resolvemos e tentamos o próximo.
	lot, err := reserveCurrentLot(ctx, tx, req.EventID, req.Quantity)
	if err != nil {
		return Result{}, err
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
	// Modelo Sympla (§4): o produtor recebe o FACE limpo; o comprador paga face + conveniência.
	face := max(subtotal-discount, 0)
	rebatePct, err := program.TierRebatePct(ctx, tx, prod.ID, time.Now())
	if err != nil {
		return Result{}, err
	}
	bd := pricing.Compute(face, req.Method, rebatePct)

	// Assento (seated): DEPOIS do lote (ordem lote→assento). Se o pagamento não confirmar,
	// a tx inteira rola atrás e a reserva do lote volta junto; a varredura de abandono
	// (inventory.Sweeper) cobre ordens que ficaram 'pending'.
	var holdID *uuid.UUID
	if seated {
		id, err := inventory.Hold(ctx, tx, req.EventID, req.SeatIDs, inventory.DefaultTTL)
		if err != nil {
			return Result{}, err
		}
		holdID = &id
	}

	// Split: o produtor recebe o FACE; a plataforma fica com a taxa de conveniência (a
	// adquirência deduz o processamento do total no ato).
	wallet := ""
	if prod.AsaasWalletID != nil {
		wallet = *prod.AsaasWalletID
	}
	split := splitInfo{ProducerCents: bd.FaceCents, PlatformCents: bd.ConvenienceFeeCents, ProducerWallet: wallet}
	splitJSON, _ := json.Marshal(split)

	// Ordem: total_cents = o que o comprador paga; guardamos a decomposição para o razão/estorno.
	var orderID uuid.UUID
	var subjectID *uuid.UUID
	if req.SubjectID != uuid.Nil {
		subjectID = &req.SubjectID
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO orders (event_id, buyer_subject_id, buyer_email, buyer_cpf, coupon_id, campaign_id, total_cents,
			face_cents, platform_fee_cents, processing_fee_cents, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'pending') RETURNING id`,
		req.EventID, subjectID, nilIfEmpty(req.BuyerEmail), nilIfEmpty(req.BuyerCPF), couponID, req.CampaignID,
		bd.TotalCents, bd.FaceCents, bd.PlatformFeeCents, bd.ProcessingCents,
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
		orderID, req.Method, installments, bd.TotalCents, string(splitJSON),
	).Scan(&paymentID); err != nil {
		return Result{}, fmt.Errorf("criar pagamento: %w", err)
	}

	// Cobrança no gateway pelo TOTAL; o split entrega o FACE ao produtor.
	var splitItems []payment.SplitItem
	if prod.AsaasWalletID != nil && *prod.AsaasWalletID != "" && split.ProducerCents > 0 {
		splitItems = []payment.SplitItem{{WalletID: *prod.AsaasWalletID, FixedCents: split.ProducerCents}}
	}
	charge, err := gw.CreateCharge(ctx, payment.ChargeRequest{
		OrderID: orderID.String(), Method: req.Method, AmountCents: bd.TotalCents, Installments: installments,
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
		OrderID: orderID, PaymentID: paymentID, AmountCents: bd.TotalCents,
		FaceCents: bd.FaceCents, ConvenienceFeeCents: bd.ConvenienceFeeCents,
		Method: req.Method, AsaasRef: charge.AsaasRef, PixCode: charge.PixCode,
	}, nil
}

// Quote calcula a decomposição de preço (face + conveniência) SEM reservar nem criar
// ordem — para a tela de checkout mostrar o total e recalcular quando o comprador troca o
// método (§4.3). Resolve o lote vigente só para precificar.
func Quote(ctx context.Context, tx pgx.Tx, producerID uuid.UUID, req Request) (pricing.Breakdown, error) {
	if req.Quantity <= 0 {
		return pricing.Breakdown{}, ErrBadRequest
	}
	if req.HalfPriceQty < 0 || req.HalfPriceQty > req.Quantity {
		return pricing.Breakdown{}, ErrBadRequest
	}
	lot, err := catalog.CurrentLot(ctx, tx, req.EventID)
	if errors.Is(err, catalog.ErrNoCurrentLot) {
		return pricing.Breakdown{}, ErrLotUnavailable
	}
	if err != nil {
		return pricing.Breakdown{}, err
	}
	unit, err := unitPrices(ctx, tx, lot, req)
	if err != nil {
		return pricing.Breakdown{}, err
	}
	subtotal := applyHalfPrice(unit, req.HalfPriceQty)
	_, discount, err := applyCoupon(ctx, tx, req.EventID, req.CouponCode, subtotal)
	if err != nil {
		return pricing.Breakdown{}, err
	}
	face := max(subtotal-discount, 0)
	rebatePct, err := program.TierRebatePct(ctx, tx, producerID, time.Now())
	if err != nil {
		return pricing.Breakdown{}, err
	}
	return pricing.Compute(face, req.Method, rebatePct), nil
}

// reserveCurrentLot resolve o lote vigente do evento e reserva `qty` nele, tolerando a
// virada que aconteça entre resolver e reservar (Correção 2). Só reserva o LOTE (trava a
// linha via UPDATE) — o assento, se houver, é travado depois pelo chamador (ordem
// lote→assento). ReserveFromLot falha por zero linhas (não por CHECK), então a tx segue
// utilizável e o retry roda DENTRO dela. Converge quando re-resolver devolve o MESMO
// lote (esgotamento genuíno para a quantidade pedida) ou pelo limite de tentativas.
func reserveCurrentLot(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, qty int) (catalog.Lot, error) {
	var lastID uuid.UUID
	for attempt := 0; attempt < maxLotResolveAttempts; attempt++ {
		lot, err := catalog.CurrentLot(ctx, tx, eventID)
		if errors.Is(err, catalog.ErrNoCurrentLot) {
			return catalog.Lot{}, ErrLotUnavailable
		}
		if err != nil {
			return catalog.Lot{}, err
		}
		rErr := catalog.ReserveFromLot(ctx, tx, lot.ID, qty)
		if rErr == nil {
			return lot, nil
		}
		if !errors.Is(rErr, catalog.ErrInsufficientStock) {
			return catalog.Lot{}, rErr
		}
		// Esgotou entre resolver e reservar. Mesmo lote duas vezes = esgotamento
		// genuíno (não cabe a quantidade neste lote); lote diferente = virou, tenta o próximo.
		if lot.ID == lastID {
			return catalog.Lot{}, ErrInsufficientStock
		}
		lastID = lot.ID
	}
	return catalog.Lot{}, ErrInsufficientStock
}

// ConfirmPayment confirma um pagamento (idempotente): emite ingressos, marca ordem
// paga e escreve o razão. Chamado pelo webhook após resolver o tenant. Devolve os
// ingressos emitidos (vazio se já estava confirmado).
func ConfirmPayment(ctx context.Context, tx pgx.Tx, em Emitter, producerID uuid.UUID, asaasRef string) ([]uuid.UUID, error) {
	var paymentID, orderID uuid.UUID
	var method, status string
	var amount int64
	err := tx.QueryRow(ctx, `
		SELECT id, order_id, method, status, amount_cents
		  FROM payments WHERE asaas_ref = $1 FOR UPDATE`, asaasRef).
		Scan(&paymentID, &orderID, &method, &status, &amount)
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
	var orderStatus string
	if err := tx.QueryRow(ctx, `SELECT event_id, coupon_id, buyer_email, status FROM orders WHERE id=$1`, orderID).Scan(&eventID, &couponID, &buyerEmail, &orderStatus); err != nil {
		return nil, err
	}
	// Ordem já cancelada/estornada (ex.: varredura de abandono liberou a reserva antes do
	// webhook chegar): não emite nem re-debita capacidade.
	if orderStatus == "cancelled" || orderStatus == "refunded" {
		return nil, nil
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

	// Confirma no LOTE primeiro (held→sold; trava a linha do lote), ANTES do assento —
	// mantém a ordem lote→assento também na confirmação. Vale para pista E seated.
	soldOut, err := catalog.ConfirmFromLot(ctx, tx, lotID, quantity)
	if err != nil {
		return nil, err
	}
	if soldOut {
		if _, err := promo.NotifyWaitlist(ctx, tx, em.Notify, eventID, "Novo lote disponível"); err != nil {
			return nil, err
		}
	}
	// O menor preço real muda quando um lote esgota — re-sincroniza o diretório (§3.10).
	if err := catalog.ResyncMinPrice(ctx, tx, eventID); err != nil {
		return nil, err
	}

	// Emite os ingressos.
	var tickets []uuid.UUID
	var holdID uuid.UUID
	holdErr := tx.QueryRow(ctx, `SELECT id FROM holds WHERE order_id=$1 AND status='held'`, orderID).Scan(&holdID)
	switch {
	case holdErr == nil: // com mapa: converte o hold (assento) em ingressos
		tickets, err = inventory.Confirm(ctx, tx, holdID, orderID, lotID, transferableAfter)
		if err != nil {
			return nil, err
		}
	case errors.Is(holdErr, pgx.ErrNoRows): // pista: emite N ingressos (capacidade já confirmada no lote)
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

	// Índice público de ingressos do comprador ("meus ingressos", sem varrer schemas).
	if err := writeTicketDirectory(ctx, tx, em.ProducerID, eventID, deliverTo, tickets); err != nil {
		return nil, err
	}

	// Apuração e razão: taxa (15% − rebate do nível), repasse (D+2) e retenção 5%/60d no
	// cartão, pelo nível vigente na data da venda; e a participação do originador.
	if err := program.SettleLedger(ctx, tx, producerID, orderID, paymentID); err != nil {
		return nil, err
	}
	return tickets, nil
}
