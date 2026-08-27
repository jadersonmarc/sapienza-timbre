// Package program lança a venda no razão: repasse ao produtor, taxa da plataforma, custo de
// processamento, retenção do cartão e a participação do originador.
//
// O PROGRAMA DE NÍVEIS FOI EXTINTO. A taxa é 10% do face para todo produtor, calculada num
// ponto só (pricing.PlatformFeeCents). Não existe rebate, tabela de níveis nem taxa efetiva
// por produtor — e nada aqui deve reintroduzi-los.
package program

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultOriginatorPct é a participação do originador sobre a fatia da plataforma.
// PROVISÓRIO: sem valor comercial definido → 0 (a originação fica inerte até definir).
const DefaultOriginatorPct = 0.0

// ErrNoFace é ordem sem decomposição de preço. Antes isso caía num fallback que apurava
// por outro modelo — e o produto passou a ter dois preços ao mesmo tempo, sem ninguém ver.
// Ordem sem face é BUG de quem a criou, não caso de negócio.
var ErrNoFace = errors.New("program: ordem sem face_cents — todo caminho de venda precisa gravar a decomposição do preço")

// SetOrigination registra (upsert) que um produtor foi indicado por um originador.
func SetOrigination(ctx context.Context, pool *pgxpool.Pool, producerID, originatorID uuid.UUID, participationPct float64, until *time.Time) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO originations (producer_id, originator_producer_id, participation_pct, effective_until)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (producer_id) DO UPDATE
		   SET originator_producer_id=EXCLUDED.originator_producer_id,
		       participation_pct=EXCLUDED.participation_pct, effective_until=EXCLUDED.effective_until`,
		producerID, originatorID, participationPct, until)
	return err
}

// SettleLedger apura a venda (usa a data da ordem = data da venda) e lança o razão:
// taxa (líquido da plataforma), repasse ao produtor (D+2), retenção 5% por 60d no cartão
// e a participação do originador (inerte enquanto a participação for 0). Sob WithTenant.
func SettleLedger(ctx context.Context, tx pgx.Tx, producerID, orderID, paymentID uuid.UUID) error {
	var eventID uuid.UUID
	var total, face, platformFee, processing int64
	if err := tx.QueryRow(ctx, `SELECT event_id, total_cents, face_cents, platform_fee_cents, processing_fee_cents FROM orders WHERE id=$1`, orderID).
		Scan(&eventID, &total, &face, &platformFee, &processing); err != nil {
		return err
	}
	// Sem decomposição não há como apurar sem inventar. Falhar aqui é barato; estimar
	// silenciosamente foi o que deixou passe de temporada e mercado secundário cobrando por
	// um modelo diferente do checkout durante meses.
	if face == 0 && total > 0 {
		return fmt.Errorf("%w (ordem %s)", ErrNoFace, orderID)
	}
	var method string
	_ = tx.QueryRow(ctx, `SELECT method FROM payments WHERE id=$1`, paymentID).Scan(&method)

	var endsAt *time.Time
	_ = tx.QueryRow(ctx, `SELECT ends_at FROM events WHERE id=$1`, eventID).Scan(&endsAt)
	repasseAt := time.Now().Add(2 * 24 * time.Hour)
	if endsAt != nil {
		repasseAt = endsAt.Add(2 * 24 * time.Hour)
	}

	// Quem entrega o face ao produtor: o gateway, quando a venda saiu com split para a
	// subconta dele, ou a plataforma, por transferência posterior. A linha do razão é a
	// mesma nos dois casos — muda só quem já pagou, e é isso que impede a fila de repasse
	// de mandar pagar de novo um valor que o gateway entregou na própria cobrança.
	settledBy := "platform"
	var splitRef *string
	if err := tx.QueryRow(ctx, `SELECT asaas_payment_id FROM split_transfers WHERE order_id=$1`, orderID).Scan(&splitRef); err == nil && splitRef != nil && *splitRef != "" {
		settledBy = "split"
	}

	// Modelo Sympla (§4.3): três linhas por venda —
	//   repasse       = valor de FACE ao produtor (D+2)
	//   taxa          = taxa de plataforma (receita da Sapienza)
	//   processamento = custo de adquirência repassado (0 enquanto provisório)
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (event_id, order_id, payment_id, kind, amount_cents, available_at, settled_by) VALUES ($1,$2,$3,'repasse',$4,$5,$6)`, eventID, orderID, paymentID, face, repasseAt, settledBy); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (event_id, order_id, payment_id, kind, amount_cents, available_at, settled_by) VALUES ($1,$2,$3,'taxa',$4, now(), $5)`, eventID, orderID, paymentID, platformFee, settledBy); err != nil {
		return err
	}
	if processing > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (event_id, order_id, payment_id, kind, amount_cents, available_at, settled_by) VALUES ($1,$2,$3,'processamento',$4, now(), $5)`, eventID, orderID, paymentID, processing, settledBy); err != nil {
			return err
		}
	}
	// Retenção antifraude no cartão — PROVISÓRIO (5% do FACE, 60d): trava parte do repasse.
	// Só morde o que a plataforma está segurando: com split, o face já saiu na cobrança e
	// não há o que reter (a linha fica registrada, marcada, e NetDue a ignora).
	if method == "credit_card" {
		ret := int64(math.Round(float64(face) * 0.05))
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (event_id, order_id, payment_id, kind, amount_cents, available_at, settled_by) VALUES ($1,$2,$3,'retencao',$4, now() + interval '60 days', $5)`, eventID, orderID, paymentID, ret, settledBy); err != nil {
			return err
		}
	}
	return RecordOrigination(ctx, tx, producerID, eventID, orderID, platformFee)
}

// RecordOrigination lança a participação do originador sobre a fatia da plataforma, se
// houver originação vigente. Enquanto participation_pct for 0 (provisório), nada é lançado.
func RecordOrigination(ctx context.Context, tx pgx.Tx, producerID, eventID, orderID uuid.UUID, platformNetCents int64) error {
	var originator uuid.UUID
	var pct float64
	var until *time.Time
	err := tx.QueryRow(ctx, `SELECT originator_producer_id, participation_pct, effective_until FROM public.originations WHERE producer_id=$1`, producerID).Scan(&originator, &pct, &until)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if until != nil && time.Now().After(*until) {
		return nil
	}
	share := int64(math.Round(float64(platformNetCents) * pct / 100))
	if share <= 0 {
		return nil
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.origination_entries (originator_producer_id, producer_id, event_id, order_id, amount_cents) VALUES ($1,$2,$3,$4,$5)`, originator, producerID, eventID, orderID, share)
	return err
}
