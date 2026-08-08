// Package market é o mercado secundário oficial (Etapa 2.2): revenda DENTRO da
// plataforma. O que hoje é cambismo vira transação registrada, com procedência e
// receita (royalty ao produtor + taxa da plataforma). A troca de titularidade em si é
// a transferência restrita (internal/transfer). Roda sob tenancy.WithTenant.
package market

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
	"github.com/jadersonmarc/sapienza-timbre/internal/program"
	"github.com/jadersonmarc/sapienza-timbre/internal/transfer"
)

var (
	// ErrAlreadyListed: já existe anúncio vivo para o ingresso.
	ErrAlreadyListed = errors.New("market: ingresso já anunciado")
	// ErrListingUnavailable: anúncio inexistente ou não está mais à venda.
	ErrListingUnavailable = errors.New("market: anúncio indisponível")
)

// Listing é um anúncio de revenda.
type Listing struct {
	ID         uuid.UUID `json:"id"`
	TicketID   uuid.UUID `json:"ticket_id"`
	PriceCents int64     `json:"price_cents"`
	Status     string    `json:"status"`
}

// CreateListing anuncia um ingresso para revenda (aplica teto; ingresso precisa estar
// ativo e fora da janela de contestação). Escreve o índice público do anúncio.
func CreateListing(ctx context.Context, tx pgx.Tx, producerID, ticketID uuid.UUID, priceCents int64) (Listing, error) {
	var eventID, lotID uuid.UUID
	var ownerWallet *uuid.UUID
	var status string
	var transferableAfter time.Time
	if err := tx.QueryRow(ctx, `
		SELECT event_id, lot_id, owner_wallet_id, status, transferable_after FROM tickets WHERE id=$1`, ticketID).
		Scan(&eventID, &lotID, &ownerWallet, &status, &transferableAfter); err != nil {
		return Listing{}, err
	}
	if status != "active" {
		return Listing{}, transfer.ErrNotActive
	}
	if time.Now().Before(transferableAfter) {
		return Listing{}, transfer.ErrNotTransferable
	}
	if err := checkCap(ctx, tx, eventID, lotID, priceCents); err != nil {
		return Listing{}, err
	}

	var l Listing
	err := tx.QueryRow(ctx, `
		INSERT INTO listings (ticket_id, price_cents, seller_wallet_id)
		VALUES ($1,$2,$3) RETURNING id, ticket_id, price_cents, status`,
		ticketID, priceCents, ownerWallet).Scan(&l.ID, &l.TicketID, &l.PriceCents, &l.Status)
	if err != nil {
		if isUnique(err) {
			return Listing{}, ErrAlreadyListed
		}
		return Listing{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO public.listing_index (listing_id, producer_id, ticket_id, price_cents, status)
		VALUES ($1,$2,$3,$4,'active')`, l.ID, producerID, ticketID, priceCents); err != nil {
		return Listing{}, err
	}
	return l, nil
}

// CancelListing cancela um anúncio ativo.
func CancelListing(ctx context.Context, tx pgx.Tx, listingID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `UPDATE listings SET status='cancelled' WHERE id=$1 AND status='active'`, listingID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrListingUnavailable
	}
	_, err = tx.Exec(ctx, `UPDATE public.listing_index SET status='cancelled' WHERE listing_id=$1`, listingID)
	return err
}

// BuyResult é o retorno da compra (o comprador paga como num checkout).
type BuyResult struct {
	ListingID  uuid.UUID `json:"listing_id"`
	OrderID    uuid.UUID `json:"order_id"`
	PriceCents int64     `json:"price_cents"`
	AsaasRef   string    `json:"asaas_ref"`
	PixCode    string    `json:"pix_code,omitempty"`
}

// BuyListing reserva o anúncio, cria a carteira do comprador, a ordem e a cobrança. A
// troca de titularidade só acontece na confirmação do pagamento (ConfirmResale).
func BuyListing(ctx context.Context, tx pgx.Tx, gw payment.PaymentGateway, producerID, listingID uuid.UUID, buyerEmail string) (BuyResult, error) {
	var ticketID uuid.UUID
	var eventID uuid.UUID
	var price int64
	var status string
	if err := tx.QueryRow(ctx, `
		SELECT l.ticket_id, l.price_cents, l.status, t.event_id
		  FROM listings l JOIN tickets t ON t.id=l.ticket_id WHERE l.id=$1 FOR UPDATE OF l`, listingID).
		Scan(&ticketID, &price, &status, &eventID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BuyResult{}, ErrListingUnavailable
		}
		return BuyResult{}, err
	}
	if status != "active" {
		return BuyResult{}, ErrListingUnavailable
	}

	// Carteira invisível do comprador (MPC real vem com a identidade; aqui um registro).
	var subjectID, walletID uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO subjects (email) VALUES ($1) RETURNING id`, nilStr(buyerEmail)).Scan(&subjectID); err != nil {
		return BuyResult{}, err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO wallets (subject_id, address) VALUES ($1,$2) RETURNING id`, subjectID, "mpc:"+subjectID.String()).Scan(&walletID); err != nil {
		return BuyResult{}, err
	}

	var orderID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO orders (event_id, buyer_email, total_cents, status)
		VALUES ($1,$2,$3,'pending') RETURNING id`, eventID, nilStr(buyerEmail), price).Scan(&orderID); err != nil {
		return BuyResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE listings SET status='reserved', buyer_order_id=$2, buyer_wallet_id=$3 WHERE id=$1`,
		listingID, orderID, walletID); err != nil {
		return BuyResult{}, err
	}
	_, _ = tx.Exec(ctx, `UPDATE public.listing_index SET status='reserved' WHERE listing_id=$1`, listingID)

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
		VALUES ($1,$2,$3,'resale') ON CONFLICT (asaas_ref) DO NOTHING`, charge.AsaasRef, producerID, orderID); err != nil {
		return BuyResult{}, err
	}
	return BuyResult{ListingID: listingID, OrderID: orderID, PriceCents: price, AsaasRef: charge.AsaasRef, PixCode: charge.PixCode}, nil
}

// ConfirmResale confirma a revenda (idempotente): transfere a titularidade ao
// comprador (restrita, com royalty), marca o anúncio vendido e lança a taxa da
// plataforma no razão. Chamado pelo webhook.
func ConfirmResale(ctx context.Context, tx pgx.Tx, producerID uuid.UUID, asaasRef string, enqueueChain bool) error {
	var paymentID, orderID uuid.UUID
	var status string
	err := tx.QueryRow(ctx, `SELECT id, order_id, status FROM payments WHERE asaas_ref=$1 FOR UPDATE`, asaasRef).Scan(&paymentID, &orderID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status == "confirmed" {
		return nil
	}
	var listingID, ticketID uuid.UUID
	var buyerWallet uuid.UUID
	var price int64
	var lStatus string
	var eventID uuid.UUID
	if err := tx.QueryRow(ctx, `
		SELECT l.id, l.ticket_id, l.buyer_wallet_id, l.price_cents, l.status, o.event_id
		  FROM listings l JOIN orders o ON o.id = l.buyer_order_id
		 WHERE l.buyer_order_id = $1`, orderID).Scan(&listingID, &ticketID, &buyerWallet, &price, &lStatus, &eventID); err != nil {
		return err
	}
	if lStatus == "reserved" {
		if _, err := transfer.Execute(ctx, tx, ticketID, buyerWallet, price, enqueueChain); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE listings SET status='sold', sold_at=now() WHERE id=$1`, listingID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE public.listing_index SET status='sold' WHERE listing_id=$1`, listingID); err != nil {
			return err
		}
		// Taxa da plataforma sobre a revenda (15% − rebate do nível); o royalty já foi
		// apurado na transferência. O repasse ao vendedor não é modelado nesta etapa.
		ap, err := program.Apurar(ctx, tx, producerID, price, time.Now())
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger_entries (event_id, order_id, payment_id, kind, amount_cents, available_at)
			VALUES ($1,$2,$3,'taxa',$4, now())`, eventID, orderID, paymentID, ap.PlatformNetCents); err != nil {
			return err
		}
		if err := program.RecordOrigination(ctx, tx, producerID, eventID, orderID, ap.PlatformNetCents); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE payments SET status='confirmed', settled_at=now() WHERE id=$1`, paymentID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE orders SET status='paid', updated_at=now() WHERE id=$1`, orderID)
	return err
}

// ── procedência ──────────────────────────────────────────────────────────────

// ChainLink é um elo da cadeia de posse.
type ChainLink struct {
	From       string    `json:"from,omitempty"`
	To         string    `json:"to,omitempty"`
	PriceCents int64     `json:"price_cents"`
	Status     string    `json:"status"`
	At         time.Time `json:"at"`
}

// Provenance é a procedência completa do ingresso.
type Provenance struct {
	EventTitle string      `json:"event_title"`
	OriginLot  string      `json:"origin_lot"`
	FaceCents  int64       `json:"face_cents"`
	Chain      []ChainLink `json:"chain"`
}

// Provenance devolve lote de origem, valor de face e a cadeia de posse do ingresso.
func GetProvenance(ctx context.Context, tx pgx.Tx, ticketID uuid.UUID) (Provenance, error) {
	var p Provenance
	if err := tx.QueryRow(ctx, `
		SELECT e.title, l.name, l.price_cents
		  FROM tickets t JOIN lots l ON l.id=t.lot_id JOIN events e ON e.id=t.event_id
		 WHERE t.id=$1`, ticketID).Scan(&p.EventTitle, &p.OriginLot, &p.FaceCents); err != nil {
		return p, err
	}
	rows, err := tx.Query(ctx, `
		SELECT COALESCE(fw.address,''), COALESCE(tw.address,''), tr.price_cents, tr.status, tr.created_at
		  FROM transfers tr
		  LEFT JOIN public.wallets fw ON fw.id = tr.from_wallet_id
		  LEFT JOIN public.wallets tw ON tw.id = tr.to_wallet_id
		 WHERE tr.ticket_id=$1 ORDER BY tr.created_at`, ticketID)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var c ChainLink
		if err := rows.Scan(&c.From, &c.To, &c.PriceCents, &c.Status, &c.At); err != nil {
			return p, err
		}
		p.Chain = append(p.Chain, c)
	}
	return p, rows.Err()
}

func checkCap(ctx context.Context, tx pgx.Tx, eventID, lotID uuid.UUID, priceCents int64) error {
	var capPct float64
	var face int64
	if err := tx.QueryRow(ctx, `SELECT resale_cap_pct FROM events WHERE id=$1`, eventID).Scan(&capPct); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT price_cents FROM lots WHERE id=$1`, lotID).Scan(&face); err != nil {
		return err
	}
	if priceCents > int64(math.Floor(float64(face)*capPct/100)) {
		return transfer.ErrPriceCap
	}
	return nil
}

func nilStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
