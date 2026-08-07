// Package transfer é o primitivo de TRANSFERÊNCIA RESTRITA (Etapa 2.1): a titularidade
// de um ingresso só muda por aqui (nunca num mercado externo livre), com teto de preço
// de revenda e royalty apurado. O registro on-chain roda em segundo plano via
// ChainDriver.Transfer (chain_job kind='transfer'). Roda sob tenancy.WithTenant.
package transfer

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrNotActive: ingresso não está ativo (queimado, cancelado, etc.).
	ErrNotActive = errors.New("transfer: ingresso não está ativo")
	// ErrNotTransferable: ainda dentro da janela de contestação (transferable_after).
	ErrNotTransferable = errors.New("transfer: ingresso ainda não é transferível")
	// ErrPriceCap: preço acima do teto de revenda do evento.
	ErrPriceCap = errors.New("transfer: preço acima do teto de revenda")
)

// Result é o resultado de uma transferência.
type Result struct {
	TransferID   uuid.UUID `json:"transfer_id"`
	PriceCents   int64     `json:"price_cents"`
	RoyaltyCents int64     `json:"royalty_cents"`
}

// Execute transfere a titularidade de um ingresso para outra carteira, aplicando teto
// e royalty. Grava o transfer + royalty_entries, reatribui o dono e (se a rede estiver
// ligada) enfileira a transferência on-chain. priceCents pode ser 0 (transferência sem
// venda), caso em que o royalty é 0.
func Execute(ctx context.Context, tx pgx.Tx, ticketID, toWalletID uuid.UUID, priceCents int64, enqueueChain bool) (Result, error) {
	if priceCents < 0 {
		return Result{}, ErrPriceCap
	}
	var eventID, lotID uuid.UUID
	var fromWallet *uuid.UUID
	var status string
	var transferableAfter time.Time
	err := tx.QueryRow(ctx, `
		SELECT event_id, lot_id, owner_wallet_id, status, transferable_after
		  FROM tickets WHERE id = $1`, ticketID).Scan(&eventID, &lotID, &fromWallet, &status, &transferableAfter)
	if err != nil {
		return Result{}, err
	}
	if status != "active" {
		return Result{}, ErrNotActive
	}
	if time.Now().Before(transferableAfter) {
		return Result{}, ErrNotTransferable
	}

	// Teto e royalty do evento (espelho das constantes do contrato).
	var capPct, royaltyPct float64
	if err := tx.QueryRow(ctx, `SELECT resale_cap_pct, royalty_pct FROM events WHERE id=$1`, eventID).Scan(&capPct, &royaltyPct); err != nil {
		return Result{}, err
	}
	var face int64
	if err := tx.QueryRow(ctx, `SELECT price_cents FROM lots WHERE id=$1`, lotID).Scan(&face); err != nil {
		return Result{}, err
	}
	maxPrice := int64(math.Floor(float64(face) * capPct / 100))
	if priceCents > maxPrice {
		return Result{}, ErrPriceCap
	}
	royalty := int64(math.Round(float64(priceCents) * royaltyPct / 100))

	var transferID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO transfers (ticket_id, from_wallet_id, to_wallet_id, price_cents, royalty_cents, status)
		VALUES ($1,$2,$3,$4,$5,'pending') RETURNING id`,
		ticketID, fromWallet, toWalletID, priceCents, royalty).Scan(&transferID); err != nil {
		return Result{}, err
	}
	if royalty > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO royalty_entries (transfer_id, amount_cents, beneficiary)
			VALUES ($1,$2,'producer')`, transferID, royalty); err != nil {
			return Result{}, err
		}
	}
	// Reatribui a titularidade (restrita: só por aqui).
	if _, err := tx.Exec(ctx, `UPDATE tickets SET owner_wallet_id=$2, updated_at=now() WHERE id=$1`, ticketID, toWalletID); err != nil {
		return Result{}, err
	}
	// Registro on-chain em segundo plano (a transferência off-chain já valeu).
	if enqueueChain {
		if _, err := tx.Exec(ctx, `INSERT INTO chain_jobs (ticket_id, kind) VALUES ($1,'transfer')`, ticketID); err != nil {
			return Result{}, err
		}
	}
	return Result{TransferID: transferID, PriceCents: priceCents, RoyaltyCents: royalty}, nil
}
