// Package payout registra a OBRIGAÇÃO de repasse: quanto a plataforma deve ao produtor de
// cada evento, e com que vencimento.
//
// A bilheteria retém o valor integral e repassa DEPOIS da realização do evento. Enquanto o
// evento não acontece, o repasse fica em `accruing` e cada venda, cortesia ou estorno o
// atualiza. Na realização ele vira `pending`, com data.
//
// A EXECUÇÃO BANCÁRIA NÃO MORA AQUI e não existe no produto: não há transferência, saque,
// validação de titularidade nem antifraude de dados bancários. O que este pacote entrega é
// o cálculo, o registro e a exibição — marcar como pago é ação manual do admin.
//
// Também não existe adiantamento antes do evento. Ele reintroduziria exatamente o risco que
// a retenção elimina: dinheiro fora da conta de quem teria de devolvê-lo.
package payout

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/checkout"
	"github.com/jadersonmarc/sapienza-timbre/internal/pricing"
)

// Estados da obrigação.
const (
	// StatusAccruing: o evento ainda não aconteceu e os valores se movem a cada venda.
	StatusAccruing = "accruing"
	// StatusPending: o evento aconteceu e o valor tem vencimento.
	StatusPending = "pending"
	// StatusOnHold: retido, com motivo e ator registrados.
	StatusOnHold = "on_hold"
	// StatusPaid: a transferência saiu (marcação manual do admin, com comprovante).
	StatusPaid = "paid"
	// StatusCancelled: evento cancelado — não há repasse, o dinheiro volta aos compradores.
	StatusCancelled = "cancelled"
)

// Motivos de retenção. São parâmetro, não texto livre: reter dinheiro de alguém por um
// motivo que não está numa lista é o tipo de decisão que ninguém consegue revisar depois.
const (
	HoldEventCancelled = "evento_cancelado"
	HoldDispute        = "disputa_aberta"
	HoldBankPending    = "verificacao_bancaria"
	HoldAdminDecision  = "decisao_admin"
)

// holdMessages é o que o PRODUTOR lê. "on_hold" sem explicação é indistinguível, para quem
// espera o dinheiro, de terem esquecido dele.
var holdMessages = map[string]string{
	HoldEventCancelled: "O evento foi cancelado e o valor está voltando para os compradores.",
	HoldDispute:        "Há uma contestação aberta em uma das compras deste evento. O repasse volta a correr quando ela for resolvida.",
	HoldBankPending:    "Faltam dados da sua conta para a transferência. Cadastre a chave Pix de recebimento.",
	HoldAdminDecision:  "O repasse está retido por decisão da plataforma. Fale com o suporte.",
}

// HoldMessage traduz o motivo para o produtor. Motivo desconhecido não vira silêncio: vira
// um texto que ao menos diz que há um motivo e onde perguntar.
func HoldMessage(reason string) string {
	if m, ok := holdMessages[reason]; ok {
		return m
	}
	if reason == "" {
		return "O repasse está retido. Fale com o suporte."
	}
	return "O repasse está retido (" + reason + "). Fale com o suporte."
}

// ValidHoldReason diz se o motivo é um dos parametrizados.
func ValidHoldReason(reason string) bool {
	_, ok := holdMessages[reason]
	return ok
}

// ErrNotFound é evento sem obrigação registrada (nenhuma venda ainda).
var ErrNotFound = errors.New("payout: evento sem repasse registrado")

// Payout é a obrigação de um evento.
type Payout struct {
	EventID           uuid.UUID `json:"event_id"`
	EventTitle        string    `json:"event_title,omitempty"`
	GrossFaceCents    int64     `json:"gross_face_cents"`
	RefundedFaceCents int64     `json:"refunded_face_cents"`
	PlatformFeeCents  int64     `json:"platform_fee_cents"`
	GatewayFeeCents   int64     `json:"gateway_fee_cents"`
	NetDueCents       int64     `json:"net_due_cents"`

	DueAt  *time.Time `json:"due_at"`
	Status string     `json:"status"`

	HoldReason  string     `json:"hold_reason,omitempty"`
	HoldMessage string     `json:"hold_message,omitempty"`
	HoldActor   string     `json:"hold_actor,omitempty"`
	HeldAt      *time.Time `json:"held_at,omitempty"`

	PaidAt        *time.Time `json:"paid_at,omitempty"`
	PaidReference string     `json:"paid_reference,omitempty"`

	// DelayDays e DelayInherited são a política aplicada: quantos dias depois da realização
	// o repasse vence, e se isso veio do padrão da casa ou do próprio evento. Mesma
	// linguagem da política de devolução — "não configurei" e "configurei igual ao padrão"
	// são estados diferentes.
	DelayDays      int  `json:"payout_delay_days"`
	DelayInherited bool `json:"payout_delay_inherited"`
}

// payoutColumns é sempre qualificado por `p`: a listagem junta com events, que também tem
// uma coluna `status`, e sem o prefixo o Postgres recusa a consulta por ambiguidade.
const payoutColumns = `p.event_id, p.gross_face_cents, p.refunded_face_cents, p.platform_fee_cents,
	p.gateway_fee_cents, p.net_due_cents, p.due_at, p.status, COALESCE(p.hold_reason,''),
	COALESCE(p.hold_actor,''), p.held_at, p.paid_at, COALESCE(p.paid_reference,'')`

func scanPayout(row pgx.Row) (Payout, error) {
	var p Payout
	err := row.Scan(&p.EventID, &p.GrossFaceCents, &p.RefundedFaceCents, &p.PlatformFeeCents,
		&p.GatewayFeeCents, &p.NetDueCents, &p.DueAt, &p.Status, &p.HoldReason,
		&p.HoldActor, &p.HeldAt, &p.PaidAt, &p.PaidReference)
	if p.Status == StatusOnHold {
		p.HoldMessage = HoldMessage(p.HoldReason)
	}
	return p, err
}

// ── prazo ─────────────────────────────────────────────────────────────────────

// DefaultDelayDays espelha o default do BANCO. Existe para o painel poder mostrar um número
// antes de qualquer configuração; a fonte é a coluna, não esta constante.
const DefaultDelayDays = 7

// Delay devolve quantos dias depois da realização o repasse do evento vence, e se o valor
// veio do padrão da casa (herdado) ou do próprio evento.
func Delay(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (int, bool, error) {
	var days int
	var found *uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT event_id, payout_delay_days FROM payout_settings
		 WHERE event_id = $1 OR event_id IS NULL
		 -- o do evento primeiro: NULLS LAST faz o padrão da casa ser o desempate
		 ORDER BY event_id NULLS LAST
		 LIMIT 1`, eventID).Scan(&found, &days)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultDelayDays, true, nil
	}
	if err != nil {
		return DefaultDelayDays, true, err
	}
	return days, found == nil, nil
}

// SetDelay grava o prazo. eventID nulo edita o padrão da casa.
func SetDelay(ctx context.Context, tx pgx.Tx, eventID *uuid.UUID, days int) error {
	if days < 0 {
		return errors.New("payout: prazo de repasse não pode ser negativo")
	}
	// O padrão da casa não tem event_id, e ON CONFLICT não trabalha com índice parcial
	// sobre expressão — os dois caminhos são escritos separados, como na política de
	// devolução.
	if eventID == nil {
		if _, err := tx.Exec(ctx, `
			UPDATE payout_settings SET payout_delay_days=$1, updated_at=now() WHERE event_id IS NULL`, days); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO payout_settings (event_id, payout_delay_days)
			SELECT NULL, $1 WHERE NOT EXISTS (SELECT 1 FROM payout_settings WHERE event_id IS NULL)`, days)
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO payout_settings (event_id, payout_delay_days) VALUES ($1, $2)
		ON CONFLICT (event_id) WHERE event_id IS NOT NULL
		DO UPDATE SET payout_delay_days = EXCLUDED.payout_delay_days, updated_at = now()`, eventID, days)
	return err
}

// ── cálculo ───────────────────────────────────────────────────────────────────

// Recompute recalcula a obrigação do evento a partir das vendas e das devoluções, e a
// grava. É idempotente e barato: chamar de novo com os mesmos dados dá o mesmo resultado.
//
// É chamado no fim de cada venda confirmada e de cada estorno, E periodicamente pelo
// Settler. A repetição é de propósito: um caminho novo que esqueça de chamar aqui produz
// uma linha desatualizada por algumas horas, não uma linha errada para sempre.
func Recompute(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (Payout, error) {
	var faceSold, feeSold int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(face_cents),0), COALESCE(SUM(platform_fee_cents),0)
		  FROM orders
		 WHERE event_id = $1 AND status IN ('paid','partially_refunded','refunded')`, eventID).
		Scan(&faceSold, &feeSold); err != nil {
		return Payout{}, err
	}
	var faceRefunded, gatewayFee int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(r.face_cents),0), COALESCE(SUM(r.gateway_fee_cents),0)
		  FROM refunds r JOIN orders o ON o.id = r.order_id
		 WHERE o.event_id = $1 AND r.status = 'confirmed'`, eventID).
		Scan(&faceRefunded, &gatewayFee); err != nil {
		return Payout{}, err
	}

	policy, err := checkout.ResolvePolicy(ctx, tx, eventID)
	if err != nil {
		return Payout{}, err
	}

	p := Payout{
		EventID:           eventID,
		GrossFaceCents:    faceSold,
		RefundedFaceCents: faceRefunded,
		GatewayFeeCents:   gatewayFee,
	}
	// A conveniência volta INTEGRAL ao comprador no estorno: a receita da plataforma
	// encolhe junto. Calculada sobre o face devolvido pela mesma fórmula que a produziu na
	// venda — é a mesma base, e estimar de outro jeito faria as duas contas divergirem.
	p.PlatformFeeCents = max(feeSold-pricing.PlatformFeeCents(faceRefunded), 0)

	p.NetDueCents = faceSold - faceRefunded
	if policy.RefundGatewayFeeBearer == checkout.FeeBearerProducer {
		p.NetDueCents -= gatewayFee
	}
	// Evento inteiro devolvido com a tarifa por conta do produtor pode deixar um resto
	// negativo. Ele fica com a plataforma: transformar um resíduo de tarifa em dívida de
	// produtor é exatamente o mecanismo que esta refatoração removeu.
	p.NetDueCents = max(p.NetDueCents, 0)

	realizedAt, realized, err := realization(ctx, tx, eventID)
	if err != nil {
		return Payout{}, err
	}
	p.DelayDays, p.DelayInherited, err = Delay(ctx, tx, eventID)
	if err != nil {
		return Payout{}, err
	}
	if realized && realizedAt != nil {
		due := realizedAt.AddDate(0, 0, p.DelayDays)
		p.DueAt = &due
	}

	// Estado atual manda: 'paid', 'cancelled' e 'on_hold' são decisões de alguém, e um
	// recálculo de rotina não desfaz decisão. O que ele sempre atualiza são os VALORES —
	// senão o extrato do produtor congela no momento em que o repasse foi retido.
	var current string
	err = tx.QueryRow(ctx, `SELECT status FROM event_payouts WHERE event_id=$1 FOR UPDATE`, eventID).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Payout{}, err
	}
	switch current {
	case StatusPaid, StatusCancelled, StatusOnHold:
		p.Status = current
	default:
		p.Status = StatusAccruing
		if realized {
			p.Status = StatusPending
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO event_payouts (event_id, gross_face_cents, refunded_face_cents,
		                           platform_fee_cents, gateway_fee_cents, net_due_cents, due_at, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (event_id) DO UPDATE
		   SET gross_face_cents    = EXCLUDED.gross_face_cents,
		       refunded_face_cents = EXCLUDED.refunded_face_cents,
		       platform_fee_cents  = EXCLUDED.platform_fee_cents,
		       gateway_fee_cents   = EXCLUDED.gateway_fee_cents,
		       net_due_cents       = EXCLUDED.net_due_cents,
		       due_at              = EXCLUDED.due_at,
		       status              = EXCLUDED.status,
		       updated_at          = now()`,
		eventID, p.GrossFaceCents, p.RefundedFaceCents, p.PlatformFeeCents,
		p.GatewayFeeCents, p.NetDueCents, p.DueAt, p.Status); err != nil {
		return Payout{}, err
	}
	if p.Status == StatusOnHold {
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(hold_reason,''), COALESCE(hold_actor,''), held_at
			  FROM event_payouts WHERE event_id=$1`, eventID).
			Scan(&p.HoldReason, &p.HoldActor, &p.HeldAt); err != nil {
			return Payout{}, err
		}
		p.HoldMessage = HoldMessage(p.HoldReason)
	}
	return p, nil
}

// realization devolve QUANDO o evento aconteceu e se já aconteceu.
//
// A realização não depende de o produtor clicar em nada: um evento que terminou ontem e
// ninguém marcou como encerrado aconteceu do mesmo jeito, e o repasse dele não pode ficar
// preso por falta de clique. 'finished' antecipa; a data resolve o resto.
func realization(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (*time.Time, bool, error) {
	var at *time.Time
	var status string
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(ends_at, starts_at), status FROM events WHERE id=$1`, eventID).Scan(&at, &status)
	if err != nil {
		return nil, false, err
	}
	if status == "cancelled" {
		return at, false, nil
	}
	realized := status == "finished" || (at != nil && !at.After(time.Now()))
	return at, realized, nil
}

// ── leitura ───────────────────────────────────────────────────────────────────

// Get devolve a obrigação de um evento, já com o prazo aplicado.
func Get(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (Payout, error) {
	p, err := scanPayout(tx.QueryRow(ctx, `SELECT `+payoutColumns+` FROM event_payouts p WHERE p.event_id=$1`, eventID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Payout{}, ErrNotFound
	}
	if err != nil {
		return Payout{}, err
	}
	p.DelayDays, p.DelayInherited, err = Delay(ctx, tx, eventID)
	return p, err
}

// List devolve os repasses do produtor, do mais recente para o mais antigo. É o histórico
// que o painel mostra: sem ele, o modelo de retenção é indistinguível de "a plataforma está
// com o meu dinheiro e não me explica nada".
func List(ctx context.Context, tx pgx.Tx) ([]Payout, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+payoutColumns+`, e.title
		  FROM event_payouts p JOIN events e ON e.id = p.event_id
		 ORDER BY COALESCE(p.due_at, e.starts_at) DESC NULLS LAST`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Payout{}
	for rows.Next() {
		var p Payout
		if err := rows.Scan(&p.EventID, &p.GrossFaceCents, &p.RefundedFaceCents, &p.PlatformFeeCents,
			&p.GatewayFeeCents, &p.NetDueCents, &p.DueAt, &p.Status, &p.HoldReason,
			&p.HoldActor, &p.HeldAt, &p.PaidAt, &p.PaidReference, &p.EventTitle); err != nil {
			return nil, err
		}
		if p.Status == StatusOnHold {
			p.HoldMessage = HoldMessage(p.HoldReason)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ── decisões ──────────────────────────────────────────────────────────────────

// Hold retém o repasse com motivo e ator. Repasse já pago não é retido: o dinheiro saiu.
func Hold(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, reason, actor string) error {
	if !ValidHoldReason(reason) {
		return errors.New("payout: motivo de retenção desconhecido: " + reason)
	}
	_, err := tx.Exec(ctx, `
		UPDATE event_payouts
		   SET status='on_hold', hold_reason=$2, hold_actor=NULLIF($3,''), held_at=now(), updated_at=now()
		 WHERE event_id=$1 AND status IN ('accruing','pending','on_hold')`, eventID, reason, actor)
	return err
}

// Release solta a retenção e devolve a linha ao cálculo normal.
func Release(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `
		UPDATE event_payouts
		   SET status='accruing', hold_reason=NULL, hold_actor=NULL, held_at=NULL, updated_at=now()
		 WHERE event_id=$1 AND status='on_hold'`, eventID); err != nil {
		return err
	}
	_, err := Recompute(ctx, tx, eventID)
	return err
}

// Cancel encerra o repasse do evento cancelado. O dinheiro está com a plataforma e volta
// aos compradores — não há o que repassar, e é aqui que o novo modelo mais ajuda: o estorno
// em massa não depende de recuperar valor de ninguém.
func Cancel(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		UPDATE event_payouts SET status='cancelled', due_at=NULL, updated_at=now()
		 WHERE event_id=$1 AND status <> 'paid'`, eventID)
	return err
}

// MarkPaid registra que a transferência saiu. AÇÃO MANUAL: não há execução bancária no
// produto. A referência é o comprovante — sem ela, "pago" vira palavra contra palavra na
// primeira divergência.
func MarkPaid(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, reference, by string) (bool, error) {
	if reference == "" {
		return false, errors.New("payout: informe a referência da transferência")
	}
	tag, err := tx.Exec(ctx, `
		UPDATE event_payouts
		   SET status='paid', paid_at=now(), paid_reference=$2, paid_by=NULLIF($3,''), updated_at=now()
		 WHERE event_id=$1 AND status IN ('pending','on_hold')`, eventID, reference, by)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// EventIDs lista os eventos com movimento financeiro — a varredura do Settler.
func EventIDs(ctx context.Context, tx pgx.Tx) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
		SELECT id FROM events
		 WHERE EXISTS (SELECT 1 FROM orders o WHERE o.event_id = events.id
		                AND o.status IN ('paid','partially_refunded','refunded'))
		    OR EXISTS (SELECT 1 FROM event_payouts p WHERE p.event_id = events.id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
