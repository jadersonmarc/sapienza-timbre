// Package program lança a venda no razão: repasse ao produtor, taxa da plataforma, custo de
// processamento e a participação do originador.
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

// SettleLedger apura a venda e lança o razão: taxa (receita da plataforma), repasse ao
// produtor, custo de processamento e a participação do originador (inerte enquanto a
// participação for 0). Sob WithTenant.
//
// NÃO EXISTE MAIS retenção de 5%/60d como reserva de contestação do produtor. Com a
// bilheteria retendo o valor até depois do evento, a reserva é da plataforma por
// construção: o dinheiro não saiu de lá para ser reservado.
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
	// Modelo de bilheteria: três linhas por venda —
	//   repasse       = valor de FACE ao produtor
	//   taxa          = taxa de plataforma (receita da Sapienza)
	//   processamento = custo de adquirência repassado (0 enquanto provisório)
	//
	// available_at fica NULO de propósito. QUANDO o produtor recebe deixou de ser
	// propriedade da linha do razão e passou a ser a obrigação de repasse do evento
	// (event_payouts.due_at): o razão diz o que aconteceu, a obrigação diz até quando. Duas
	// datas para a mesma promessa é como elas divergem.
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (event_id, order_id, payment_id, kind, amount_cents) VALUES ($1,$2,$3,'repasse',$4)`, eventID, orderID, paymentID, face); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (event_id, order_id, payment_id, kind, amount_cents, available_at) VALUES ($1,$2,$3,'taxa',$4, now())`, eventID, orderID, paymentID, platformFee); err != nil {
		return err
	}
	if processing > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (event_id, order_id, payment_id, kind, amount_cents, available_at) VALUES ($1,$2,$3,'processamento',$4, now())`, eventID, orderID, paymentID, processing); err != nil {
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
