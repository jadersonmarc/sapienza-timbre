// Package program é o programa de produtores (Etapa 2.7): níveis, apuração da taxa por
// nível vigente NA DATA DA VENDA, e originação. Valores DEFINIDOS: taxa 15%; níveis
// Iniciante 10% / Pro 15% / Sênior 20% (versionados em public.platform_fee_rules).
//
// PROVISÓRIO (pendente de definição comercial — não inventado, isolado e reportado):
//   - producerRebate: como o % do nível modifica a taxa. Interpretação de trabalho: o
//     produtor RETÉM tier_pct% da taxa (rebate). Trocar aqui quando definido.
//   - DefaultOriginatorPct: participação do originador. Sem valor → 0 (inerte).
//   - Critério de transição entre níveis: não automático; troca manual (SetTier).
package program

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultOriginatorPct é a participação do originador sobre a fatia da plataforma.
// PROVISÓRIO: sem valor comercial definido → 0 (a originação fica inerte até definir).
const DefaultOriginatorPct = 0.0

// producerRebate calcula quanto da taxa volta ao produtor pelo nível dele.
// PROVISÓRIO: fórmula tier→benefício pendente de definição comercial (interpretação de
// trabalho = produtor retém tier_pct% da taxa).
func producerRebate(feeCents int64, tierPct float64) int64 {
	return int64(math.Round(float64(feeCents) * tierPct / 100))
}

// Queryer é o subconjunto usado por Apurar/TierAt (satisfeito por *pgxpool.Pool e pgx.Tx).
type Queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Apuracao é o resultado da apuração de uma venda.
type Apuracao struct {
	Tier                string  `json:"tier"`
	FeePct              float64 `json:"fee_pct"`
	TierPct             float64 `json:"tier_pct"`
	FeeCents            int64   `json:"fee_cents"`
	ProducerRebateCents int64   `json:"producer_rebate_cents"`
	PlatformNetCents    int64   `json:"platform_net_cents"`
}

// Apurar calcula a taxa (15%), o rebate do nível (PROVISÓRIO) e o líquido da plataforma,
// usando as regras e o nível vigentes na data `at` (a data da venda).
func Apurar(ctx context.Context, db Queryer, producerID uuid.UUID, valueCents int64, at time.Time) (Apuracao, error) {
	var feePct, ini, pro, sen float64
	if err := db.QueryRow(ctx, `
		SELECT fee_pct, tier_iniciante_pct, tier_pro_pct, tier_senior_pct
		  FROM public.platform_fee_rules
		 WHERE effective_from <= $1 ORDER BY effective_from DESC LIMIT 1`, at).Scan(&feePct, &ini, &pro, &sen); err != nil {
		return Apuracao{}, err
	}
	tier := TierAt(ctx, db, producerID, at)
	tierPct := ini
	switch tier {
	case "pro":
		tierPct = pro
	case "senior":
		tierPct = sen
	}
	fee := int64(math.Round(float64(valueCents) * feePct / 100))
	rebate := producerRebate(fee, tierPct)
	return Apuracao{
		Tier: tier, FeePct: feePct, TierPct: tierPct,
		FeeCents: fee, ProducerRebateCents: rebate, PlatformNetCents: fee - rebate,
	}, nil
}

// TierRebatePct devolve o percentual do NÍVEL vigente do produtor na data `at` (o rebate
// do programa). Usado pelo modelo de cobrança para descontar da taxa de plataforma.
func TierRebatePct(ctx context.Context, db Queryer, producerID uuid.UUID, at time.Time) (float64, error) {
	var ini, pro, sen float64
	if err := db.QueryRow(ctx, `
		SELECT tier_iniciante_pct, tier_pro_pct, tier_senior_pct
		  FROM public.platform_fee_rules
		 WHERE effective_from <= $1 ORDER BY effective_from DESC LIMIT 1`, at).Scan(&ini, &pro, &sen); err != nil {
		return 0, err
	}
	switch TierAt(ctx, db, producerID, at) {
	case "pro":
		return pro, nil
	case "senior":
		return sen, nil
	default:
		return ini, nil
	}
}

// TierAt devolve o nível vigente do produtor na data `at` (histórico ou atual).
func TierAt(ctx context.Context, db Queryer, producerID uuid.UUID, at time.Time) string {
	var tier string
	if err := db.QueryRow(ctx, `
		SELECT tier FROM public.producer_tier_history
		 WHERE producer_id=$1 AND effective_from<=$2 ORDER BY effective_from DESC LIMIT 1`, producerID, at).Scan(&tier); err == nil {
		return tier
	}
	_ = db.QueryRow(ctx, `SELECT tier FROM public.producers WHERE id=$1`, producerID).Scan(&tier)
	if tier == "" {
		tier = "iniciante"
	}
	return tier
}

var errBadTier = errors.New("program: nível inválido")

// SetTier registra uma transição de nível com vigência (nunca retroativa sobre o que já
// foi apurado — a apuração sempre usa o nível vigente na data da venda).
func SetTier(ctx context.Context, pool *pgxpool.Pool, producerID uuid.UUID, tier string, effectiveFrom time.Time) error {
	if tier != "iniciante" && tier != "pro" && tier != "senior" {
		return errBadTier
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO producer_tier_history (producer_id, tier, effective_from) VALUES ($1,$2,$3)`, producerID, tier, effectiveFrom); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE producers SET tier=$2, updated_at=now() WHERE id=$1`, producerID, tier); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

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
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `SELECT event_id, total_cents, face_cents, platform_fee_cents, processing_fee_cents, created_at FROM orders WHERE id=$1`, orderID).
		Scan(&eventID, &total, &face, &platformFee, &processing, &createdAt); err != nil {
		return err
	}
	// Fallback legado (§4 não migrou season/mercado): ordem sem decomposição usa o modelo
	// antigo (taxa embutida via Apurar sobre o total), preservando esses fluxos.
	if face == 0 && total > 0 {
		ap, err := Apurar(ctx, tx, producerID, total, createdAt)
		if err != nil {
			return err
		}
		platformFee = ap.PlatformNetCents
		face = total - platformFee
		processing = 0
	}
	var method string
	_ = tx.QueryRow(ctx, `SELECT method FROM payments WHERE id=$1`, paymentID).Scan(&method)

	var endsAt *time.Time
	_ = tx.QueryRow(ctx, `SELECT ends_at FROM events WHERE id=$1`, eventID).Scan(&endsAt)
	repasseAt := time.Now().Add(2 * 24 * time.Hour)
	if endsAt != nil {
		repasseAt = endsAt.Add(2 * 24 * time.Hour)
	}

	// Modelo Sympla (§4.3): três linhas por venda —
	//   repasse       = valor de FACE ao produtor (D+2)
	//   taxa          = taxa de plataforma (receita da Sapienza)
	//   processamento = custo de adquirência repassado (0 enquanto provisório)
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (event_id, order_id, payment_id, kind, amount_cents, available_at) VALUES ($1,$2,$3,'repasse',$4,$5)`, eventID, orderID, paymentID, face, repasseAt); err != nil {
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
	// Retenção antifraude no cartão — PROVISÓRIO (5% do FACE, 60d): trava parte do repasse.
	if method == "credit_card" {
		ret := int64(math.Round(float64(face) * 0.05))
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (event_id, order_id, payment_id, kind, amount_cents, available_at) VALUES ($1,$2,$3,'retencao',$4, now() + interval '60 days')`, eventID, orderID, paymentID, ret); err != nil {
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
