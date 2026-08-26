package checkout

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
	"github.com/jadersonmarc/sapienza-timbre/internal/pricing"
)

// Status do split no gateway.
const (
	SplitPending        = "PENDING"
	SplitAwaitingCredit = "AWAITING_CREDIT"
	SplitCancelled      = "CANCELLED"
	SplitDone           = "DONE"
	SplitRefused        = "REFUSED"
	SplitRefunded       = "REFUNDED"
	// SplitBlocked não é status do gateway: é nosso, para o bloqueio por divergência, que
	// tem prazo para resolver e precisa aparecer como trabalho pendente.
	SplitBlocked = "BLOCKED"
)

// recordSplitTransfer registra o repasse combinado com o produtor, junto da tabela de
// tarifas usada no cálculo. O snapshot é o que permite explicar, meses depois, por que a
// cobrança teve aquele valor — a tabela pode ter mudado desde então.
func recordSplitTransfer(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, producerID uuid.UUID,
	eventID uuid.UUID, bd pricing.Breakdown, method string, installments int, asaasRef string, fees payment.Fees) error {
	snapshot, err := json.Marshal(fees)
	if err != nil {
		snapshot = []byte(`{}`)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO split_transfers
		    (order_id, event_id, producer_id, face_cents, convenience_cents, platform_margin_cents,
		     payment_method, installments, asaas_payment_id, split_status, fee_snapshot)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (order_id) DO UPDATE
		    SET asaas_payment_id = EXCLUDED.asaas_payment_id, updated_at = now()`,
		orderID, eventID, producerID, bd.FaceCents, bd.ConvenienceFeeCents, bd.PlatformFeeCents,
		method, installments, nilIfEmpty(asaasRef), SplitPending, snapshot)
	if err != nil {
		return fmt.Errorf("registrar repasse: %w", err)
	}
	return nil
}

// MarkSplitStatus move o status do repasse a partir do webhook. splitID chega no evento e é
// guardado para o caso de uma cobrança gerar mais de um split.
func MarkSplitStatus(ctx context.Context, tx pgx.Tx, asaasPaymentID, splitID, status, reason string) error {
	_, err := tx.Exec(ctx, `
		UPDATE split_transfers
		   SET split_status = $3,
		       asaas_split_id = COALESCE(NULLIF($2,''), asaas_split_id),
		       refusal_reason = NULLIF($4,''),
		       updated_at = now()
		 WHERE asaas_payment_id = $1`, asaasPaymentID, splitID, status, reason)
	return err
}

// LineupShare é a fatia informativa de um artista do line-up.
type LineupShare struct {
	ArtistName string  `json:"artist_name"`
	SharePct   float64 `json:"share_pct"`
	// AmountCents é o previsto sobre o face vendido — cálculo do painel, não movimento
	// de dinheiro: quem paga o artista é o produtor, por fora.
	AmountCents int64 `json:"amount_cents"`
}

// LineupPreview calcula o rateio previsto do line-up sobre o face já vendido no evento. É
// informativo: nenhum artista é recebedor no gateway.
func LineupPreview(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) ([]LineupShare, error) {
	var faceSold int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(face_cents),0) FROM split_transfers
		 WHERE event_id=$1 AND split_status <> $2`, eventID, SplitRefunded).Scan(&faceSold); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT artist_name, share_pct FROM lineup_shares WHERE event_id=$1 ORDER BY share_pct DESC`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LineupShare
	for rows.Next() {
		var s LineupShare
		if err := rows.Scan(&s.ArtistName, &s.SharePct); err != nil {
			return nil, err
		}
		s.AmountCents = int64(float64(faceSold) * s.SharePct / 100)
		out = append(out, s)
	}
	return out, rows.Err()
}
