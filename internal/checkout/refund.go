package checkout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/catalog"
)

// Erros do estorno que o chamador precisa distinguir.
var (
	// ErrRefundNothing é ordem sem nada a estornar (já estornada, ou os ingressos pedidos
	// não estão mais ativos).
	ErrRefundNothing = errors.New("nada a estornar nesta ordem")
	// ErrRefundInFlight é ingresso já alcançado por um estorno vivo — duplo clique, retry
	// concorrente. Barrado por índice único, não pela aplicação.
	ErrRefundInFlight = errors.New("já existe um estorno em andamento para este ingresso")
	// ErrRefundCheckedIn é ingresso que já entrou. Quem usou o ingresso consumiu o serviço;
	// devolver o dinheiro é decisão da plataforma, não operação de rotina do produtor.
	ErrRefundCheckedIn = errors.New("ingresso com entrada registrada não é estornável")
	// ErrRefundNotPaid é ordem que não chegou a ser paga.
	ErrRefundNotPaid = errors.New("ordem não paga")
)

// Origem da operação de estorno.
const (
	RefundByWebhook  = "webhook"
	RefundByProducer = "producer"
	RefundByAdmin    = "admin"
)

// De onde saiu o dinheiro devolvido ao comprador (documentado na migration 0022).
const (
	SourceNotSettled      = "not_settled"
	SourcePlatformBalance = "platform_balance"
	SourceProducer        = "producer"
	SourcePlatformCovered = "platform_covered"
)

// webhookEchoWindow é por quanto tempo o aviso de estorno do gateway é lido como ECO de um
// estorno que nós mesmos originamos, e não como um estorno feito por fora.
//
// PROVISÓRIO. O ideal seria casar pelo id do estorno, mas o aviso do gateway não o traz de
// forma confiável — e sem janela, o eco do nosso próprio estorno seria tratado como um
// estorno externo e queimaria os ingressos que sobraram no pedido.
const webhookEchoWindow = 10 * time.Minute

// RefundLine é um ingresso do estorno e o face que cabe a ele.
type RefundLine struct {
	TicketID  uuid.UUID
	FaceCents int64
}

// RefundInput descreve o que estornar.
type RefundInput struct {
	OrderID uuid.UUID
	// TicketIDs vazio = todos os ingressos ativos da ordem (estorno total).
	TicketIDs   []uuid.UUID
	InitiatedBy string
	Reason      string
	// AllowCheckedIn libera o estorno de ingresso que já entrou. É o passe de administração:
	// o produtor não tem essa porta.
	AllowCheckedIn bool
}

// PreparedRefund é o estorno registrado e ainda não executado no gateway.
type PreparedRefund struct {
	ID        uuid.UUID
	OrderID   uuid.UUID
	PaymentID uuid.UUID
	AsaasRef  string
	Scope     string
	Lines     []RefundLine
	// FaceCents volta ao comprador pelo produtor; ConvenienceCents, pela plataforma.
	FaceCents        int64
	ConvenienceCents int64
	TotalCents       int64
	// GatewayFeeCents é a tarifa que o gateway retém e não devolve — custo da plataforma.
	GatewayFeeCents int64
}

// PrepareRefund registra a intenção de estornar e reserva os ingressos, em transação curta.
//
// É a primeira das duas fases. A chamada ao gateway NÃO pode acontecer aqui: se o dinheiro
// volta e a transação faz rollback, o comprador foi estornado e o ingresso continua válido,
// sem registro nenhum. Uma cobrança órfã é inofensiva; um estorno órfão é dinheiro perdido.
func PrepareRefund(ctx context.Context, tx pgx.Tx, in RefundInput) (PreparedRefund, error) {
	var out PreparedRefund
	out.OrderID = in.OrderID

	var orderStatus string
	var orderFace, orderTotal, orderProcessing int64
	err := tx.QueryRow(ctx, `
		SELECT status, face_cents, total_cents, processing_fee_cents FROM orders WHERE id=$1 FOR UPDATE`, in.OrderID).
		Scan(&orderStatus, &orderFace, &orderTotal, &orderProcessing)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrRefundNothing
	}
	if err != nil {
		return out, err
	}
	if orderStatus == "refunded" {
		return out, ErrRefundNothing // já devolvida por inteiro
	}
	if orderStatus != "paid" && orderStatus != "partially_refunded" {
		return out, ErrRefundNotPaid
	}

	var paymentID uuid.UUID
	var asaasRef *string
	if err := tx.QueryRow(ctx, `
		SELECT id, asaas_ref FROM payments WHERE order_id=$1 ORDER BY created_at DESC LIMIT 1`, in.OrderID).
		Scan(&paymentID, &asaasRef); err != nil {
		return out, fmt.Errorf("pagamento da ordem: %w", err)
	}
	out.PaymentID = paymentID
	if asaasRef != nil {
		out.AsaasRef = *asaasRef
	}

	// Face por ingresso: o preço do item que o ingresso ocupa, ajustado para fechar
	// exatamente o face da ordem (o cupom desconta a ordem, não o item).
	all, err := ticketFaces(ctx, tx, in.OrderID, orderFace)
	if err != nil {
		return out, err
	}
	if len(all) == 0 {
		return out, ErrRefundNothing
	}

	lines := all
	if len(in.TicketIDs) > 0 {
		want := make(map[uuid.UUID]bool, len(in.TicketIDs))
		for _, id := range in.TicketIDs {
			want[id] = true
		}
		lines = lines[:0:0]
		for _, l := range all {
			if want[l.TicketID] {
				lines = append(lines, l)
			}
		}
		if len(lines) != len(want) {
			return out, ErrRefundNothing
		}
	}
	if len(lines) == 0 {
		return out, ErrRefundNothing
	}

	if !in.AllowCheckedIn {
		if err := rejectCheckedIn(ctx, tx, lines); err != nil {
			return out, err
		}
	}

	out.Scope = "partial"
	if len(lines) == len(all) {
		out.Scope = "total"
	}
	out.Lines = lines
	for _, l := range lines {
		out.FaceCents += l.FaceCents
	}
	// A conveniência volta integral, na proporção do face estornado: ninguém lucra com
	// cancelamento. No estorno total isso é o valor cheio, sem sobra de centavo.
	convenience := orderTotal - orderFace
	if out.Scope == "total" {
		out.ConvenienceCents = convenience - refundedConvenience(ctx, tx, in.OrderID)
	} else if orderFace > 0 {
		out.ConvenienceCents = mulDiv(convenience, out.FaceCents, orderFace)
	}
	if out.ConvenienceCents < 0 {
		out.ConvenienceCents = 0
	}
	out.TotalCents = out.FaceCents + out.ConvenienceCents
	// A tarifa do gateway NÃO volta: é custo da plataforma. Registrada na proporção do que
	// foi devolvido, com a mesma estimativa que entrou no preço da venda — sem ela, o custo
	// real do estorno some do registro.
	if orderFace > 0 {
		out.GatewayFeeCents = mulDiv(orderProcessing, out.FaceCents, orderFace)
	}

	ids := make([]uuid.UUID, 0, len(lines))
	for _, l := range lines {
		ids = append(ids, l.TicketID)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO refunds (order_id, payment_id, scope, ticket_ids, face_cents, convenience_cents,
		                     gateway_fee_cents, total_cents, status, initiated_by, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'pending',$9,$10) RETURNING id`,
		in.OrderID, paymentID, out.Scope, ids, out.FaceCents, out.ConvenienceCents,
		out.GatewayFeeCents, out.TotalCents, in.InitiatedBy, nilIfEmpty(in.Reason)).Scan(&out.ID); err != nil {
		return out, fmt.Errorf("registrar estorno: %w", err)
	}
	for _, l := range lines {
		// O índice único parcial em refund_tickets é quem barra o duplo clique e o retry
		// concorrente — a checagem não é da aplicação.
		if _, err := tx.Exec(ctx, `
			INSERT INTO refund_tickets (refund_id, ticket_id, face_cents) VALUES ($1,$2,$3)`,
			out.ID, l.TicketID, l.FaceCents); err != nil {
			if isUniqueViolation(err) {
				return out, ErrRefundInFlight
			}
			return out, fmt.Errorf("reservar ingresso do estorno: %w", err)
		}
	}
	return out, nil
}

// MarkRefundSent registra que o gateway aceitou a devolução. Entre esta marca e o
// CompleteRefund existe uma janela em que o dinheiro já voltou e o efeito ainda não foi
// aplicado — é justamente para essa janela que a linha em refunds existe.
func MarkRefundSent(ctx context.Context, tx pgx.Tx, refundID uuid.UUID, gatewayRefundID string) error {
	_, err := tx.Exec(ctx, `
		UPDATE refunds SET status='sent', gateway_refund_id=NULLIF($2,''), updated_at=now()
		 WHERE id=$1`, refundID, gatewayRefundID)
	return err
}

// FailRefund encerra a operação sem efeito e devolve os ingressos: eles continuam válidos e
// podem entrar em outro estorno. Falhar sem soltar os ingressos deixaria o pedido travado.
func FailRefund(ctx context.Context, tx pgx.Tx, refundID uuid.UUID, cause string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE refunds SET status='failed', error=$2, updated_at=now() WHERE id=$1`, refundID, cause); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE refund_tickets SET dead=true WHERE refund_id=$1`, refundID)
	return err
}

// CompleteRefund aplica os efeitos do estorno: devolve capacidade ao lote, libera assentos,
// QUEIMA os ingressos, reflete nos índices públicos, reverte o repasse e lança o razão.
// Idempotente por status.
//
// coveredByPlatform é o caso em que a subconta do produtor não cobriu a devolução: o
// comprador foi estornado assim mesmo, e o produtor ficou devendo. Precisa entrar AQUI, e
// não numa correção posterior, porque é isso que decide o sinal do razão — marcar depois
// deixaria a dívida invisível em NetDue.
func CompleteRefund(ctx context.Context, tx pgx.Tx, refundID uuid.UUID, coveredByPlatform bool) error {
	var orderID, paymentID uuid.UUID
	var status, scope string
	var face, convenience int64
	err := tx.QueryRow(ctx, `
		SELECT order_id, payment_id, status, scope, face_cents, convenience_cents
		  FROM refunds WHERE id=$1 FOR UPDATE`, refundID).
		Scan(&orderID, &paymentID, &status, &scope, &face, &convenience)
	if err != nil {
		return err
	}
	if status == "confirmed" {
		return nil // idempotente
	}

	var eventID uuid.UUID
	var orderFace int64
	if err := tx.QueryRow(ctx, `SELECT event_id, face_cents FROM orders WHERE id=$1 FOR UPDATE`, orderID).
		Scan(&eventID, &orderFace); err != nil {
		return err
	}

	tickets, err := refundTicketIDs(ctx, tx, refundID)
	if err != nil {
		return err
	}

	// Ordem lote→assento, a mesma da venda: devolve capacidade ao(s) lote(s) ANTES de
	// mexer nos assentos. RefundFromLot tem piso em 0 — estorno duplicado não leva o
	// contador a negativo.
	byLot, err := tx.Query(ctx, `
		SELECT lot_id, count(*) FROM tickets
		 WHERE id = ANY($1) AND status='active' GROUP BY lot_id`, tickets)
	if err != nil {
		return err
	}
	type lotQty struct {
		lot uuid.UUID
		qty int
	}
	var refunds []lotQty
	for byLot.Next() {
		var lq lotQty
		if err := byLot.Scan(&lq.lot, &lq.qty); err != nil {
			byLot.Close()
			return err
		}
		refunds = append(refunds, lq)
	}
	byLot.Close()
	if err := byLot.Err(); err != nil {
		return err
	}
	for _, lq := range refunds {
		if err := catalog.RefundFromLot(ctx, tx, lq.lot, lq.qty); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE seat_occupancy SET released = true
		 WHERE ticket_id IN (SELECT id FROM tickets WHERE id = ANY($1) AND status='active')`, tickets); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tickets SET status='burned', updated_at=now()
		 WHERE id = ANY($1) AND status='active'`, tickets); err != nil {
		return err
	}
	// Pontos de escrita do índice público (§3.10): quem procura "por que meu ingresso
	// sumiu" acha a resposta no histórico, não no vazio.
	if _, err := tx.Exec(ctx, `
		UPDATE ticket_directory SET status='refunded' WHERE ticket_id = ANY($1)`, tickets); err != nil {
		return err
	}

	// A ordem só vira 'refunded' quando não sobra ingresso ativo. Enquanto sobrar, é
	// 'partially_refunded' — e os que sobraram continuam valendo na portaria.
	var remaining int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tickets WHERE order_id=$1 AND status='active'`, orderID).
		Scan(&remaining); err != nil {
		return err
	}
	orderStatus := "partially_refunded"
	if remaining == 0 {
		orderStatus = "refunded"
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET status=$2, updated_at=now() WHERE id=$1`, orderID, orderStatus); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE order_directory SET status=$2, refunded_at=COALESCE(refunded_at, now())
		 WHERE order_id=$1`, orderID, orderStatus); err != nil {
		return err
	}
	if remaining == 0 {
		if _, err := tx.Exec(ctx, `UPDATE payments SET status='refunded', updated_at=now() WHERE id=$1`, paymentID); err != nil {
			return err
		}
	}

	// Reversão do repasse: de onde saiu o dinheiro decide quem fica devendo.
	source, err := reverseSplit(ctx, tx, refundID, orderID, tickets, remaining == 0, coveredByPlatform)
	if err != nil {
		return err
	}

	if err := writeRefundLedger(ctx, tx, eventID, orderID, paymentID, face, convenience, orderFace, source); err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `UPDATE refunds SET status='confirmed', updated_at=now() WHERE id=$1`, refundID)
	return err
}

// writeRefundLedger lança o estorno no razão. São três linhas, espelhando as três da venda:
//
//	estorno      = -face          — o que volta pelo produtor
//	estorno_taxa = -conveniência  — o que volta pela plataforma
//	retencao     = -proporcional  — desfaz a reserva de contestação da parte estornada
//
// A terceira é a que passa despercebido: NetDue SUBTRAI a retenção enquanto ela está
// retida. Sem desfazê-la, uma venda estornada dentro dos 60 dias deixaria o produtor com
// saldo negativo por uma venda que não existe mais. Lançamento negativo com o mesmo
// available_at zera a soma sem UPDATE destrutivo — o razão continua append-only.
func writeRefundLedger(ctx context.Context, tx pgx.Tx, eventID, orderID, paymentID uuid.UUID,
	face, convenience, orderFace int64, source string) error {
	// settled_by do estorno acompanha de onde o dinheiro saiu: quando ele volta da subconta
	// do produtor (ou o split nem tinha liquidado), a plataforma não passa a dever nem a
	// receber nada — as duas pontas se cancelam e a linha fica fora de NetDue. Quando a
	// plataforma cobre, o produtor fica devendo, e é NetDue que tem de refletir isso.
	settledBy := "platform"
	if source == SourceNotSettled || source == SourceProducer {
		settledBy = "split"
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries (event_id, order_id, payment_id, kind, amount_cents, available_at, settled_by)
		VALUES ($1,$2,$3,'estorno',$4, now(), $5)`, eventID, orderID, paymentID, -face, settledBy); err != nil {
		return err
	}
	if convenience > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger_entries (event_id, order_id, payment_id, kind, amount_cents, available_at, settled_by)
			VALUES ($1,$2,$3,'estorno_taxa',$4, now(), 'platform')`, eventID, orderID, paymentID, -convenience); err != nil {
			return err
		}
	}
	return neutralizeRetention(ctx, tx, orderID, face, orderFace)
}

// neutralizeRetention desfaz a fatia da retenção correspondente ao face estornado. Trabalha
// sobre o SALDO da retenção da ordem (positivos menos negativos já lançados), então
// estornos parciais sucessivos fecham em zero sem sobra de centavo.
func neutralizeRetention(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, face, orderFace int64) error {
	var gross, net int64
	var availableAt *time.Time
	var settledBy string
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_cents) FILTER (WHERE amount_cents > 0),0),
		       COALESCE(SUM(amount_cents),0),
		       MIN(available_at) FILTER (WHERE amount_cents > 0),
		       COALESCE(MIN(settled_by) FILTER (WHERE amount_cents > 0),'platform')
		  FROM ledger_entries WHERE order_id=$1 AND kind='retencao'`, orderID).
		Scan(&gross, &net, &availableAt, &settledBy)
	if err != nil || gross == 0 || net <= 0 {
		return err
	}
	undo := net // estorno total (ou o que sobrou): zera o saldo
	if orderFace > 0 && face < orderFace {
		undo = min(mulDiv(gross, face, orderFace), net)
	}
	if undo <= 0 {
		return nil
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO ledger_entries (event_id, order_id, kind, amount_cents, available_at, settled_by)
		SELECT event_id, $1, 'retencao', $2, $3, $4 FROM orders WHERE id=$1`,
		orderID, -undo, availableAt, settledBy)
	return err
}

// reverseSplit reverte o repasse ao produtor pelos ingressos estornados e devolve de onde o
// dinheiro saiu. Três estados, tratamento diferente em cada — e o parcial NÃO pode
// sobrescrever split_transfers, que tem uma linha por PEDIDO: um ingresso de quatro
// derrubaria o repasse inteiro.
func reverseSplit(ctx context.Context, tx pgx.Tx, refundID, orderID uuid.UUID, tickets []uuid.UUID, whole, covered bool) (string, error) {
	var splitStatus string
	var splitRef *string
	err := tx.QueryRow(ctx, `
		SELECT split_status, asaas_payment_id FROM split_transfers WHERE order_id=$1 FOR UPDATE`, orderID).
		Scan(&splitStatus, &splitRef)
	source := SourcePlatformBalance
	switch {
	case errors.Is(err, pgx.ErrNoRows) || splitRef == nil || *splitRef == "":
		// Venda centralizada: o dinheiro nunca saiu da plataforma. Não há split a reverter,
		// e o razão simplesmente passa a dever menos.
		source = SourcePlatformBalance
	case err != nil:
		return "", err
	case splitStatus == SplitPending || splitStatus == SplitAwaitingCredit:
		// Ainda não liquidou: o estorno da cobrança cancela o split junto. Nada a recuperar.
		source = SourceNotSettled
	default:
		// Liquidado: o gateway puxou o valor da subconta do produtor ao estornar a cobrança.
		source = SourceProducer
	}
	if covered {
		// A subconta não cobriu. Quem devolveu foi a plataforma, e é ela que passa a ter
		// crédito contra o produtor.
		source = SourcePlatformCovered
	}

	for _, id := range tickets {
		if _, err := tx.Exec(ctx, `
			INSERT INTO split_refunds (refund_id, order_id, ticket_id, face_cents, source)
			SELECT $1, $2, ticket_id, face_cents, $3 FROM refund_tickets
			 WHERE refund_id=$1 AND ticket_id=$4
			ON CONFLICT (refund_id, ticket_id) DO NOTHING`, refundID, orderID, source, id); err != nil {
			return "", fmt.Errorf("registrar reversão do repasse: %w", err)
		}
	}
	// split_transfers só vai a REFUNDED quando não sobra face no pedido.
	if whole && source != SourcePlatformBalance && splitRef != nil {
		if err := MarkSplitStatus(ctx, tx, *splitRef, "", SplitRefunded, ""); err != nil {
			return "", err
		}
	}
	return source, nil
}

// RefundPayment processa o aviso de estorno vindo do gateway (contestação no cartão,
// devolução feita no painel do Asaas). Estorna o que ainda estiver ativo na ordem.
// Idempotente: aviso repetido, e eco do estorno que nós mesmos originamos, não reprocessam.
func RefundPayment(ctx context.Context, tx pgx.Tx, asaasRef string) error {
	var orderID uuid.UUID
	var status string
	err := tx.QueryRow(ctx, `
		SELECT p.order_id, p.status FROM payments p
		 WHERE p.asaas_ref = $1 FOR UPDATE OF p`, asaasRef).Scan(&orderID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // desconhecido: ignora
	}
	if err != nil {
		return err
	}
	if status == "refunded" {
		return nil // idempotente
	}
	// Eco do nosso próprio estorno: o gateway avisa o que nós mandamos fazer. Sem esta
	// guarda, o aviso seria lido como devolução externa e queimaria o que sobrou do pedido.
	var echoes int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM refunds
		 WHERE order_id=$1 AND initiated_by <> 'webhook' AND status IN ('sent','confirmed')
		   AND updated_at > now() - $2::interval`, orderID, webhookEchoWindow.String()).Scan(&echoes); err != nil {
		return err
	}
	if echoes > 0 {
		return nil
	}

	prepared, err := PrepareRefund(ctx, tx, RefundInput{
		OrderID:     orderID,
		InitiatedBy: RefundByWebhook,
		Reason:      "estorno informado pelo gateway",
		// O gateway já devolveu o dinheiro: recusar aqui por causa de uma entrada
		// registrada deixaria o comprador estornado com o ingresso valendo.
		AllowCheckedIn: true,
	})
	if errors.Is(err, ErrRefundNothing) || errors.Is(err, ErrRefundNotPaid) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := MarkRefundSent(ctx, tx, prepared.ID, ""); err != nil {
		return err
	}
	return CompleteRefund(ctx, tx, prepared.ID, false)
}

// ── apoio ────────────────────────────────────────────────────────────────────

// ticketFaces distribui o face da ordem entre os ingressos ATIVOS, usando o preço do item
// que cada um ocupa como peso. O preço do item não serve sozinho: o cupom desconta a ORDEM,
// não o item, e assento guarda o preço cheio com a meia como marca (pista já guarda o
// preço com a meia aplicada). A sobra da divisão vai para os ingressos de maior peso, então
// a soma fecha o face exato — nenhum centavo aparece nem some.
func ticketFaces(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, orderFace int64) ([]RefundLine, error) {
	rows, err := tx.Query(ctx, `
		SELECT t.id,
		       COALESCE(
		         (SELECT CASE WHEN oi.half_price AND oi.seat_id IS NOT NULL
		                      THEN oi.unit_price_cents / 2 ELSE oi.unit_price_cents END
		            FROM order_items oi
		           WHERE oi.order_id = t.order_id
		             AND (oi.seat_id = t.seat_id
		                  OR (t.seat_id IS NULL AND oi.seat_id IS NULL AND oi.half_price = t.half_price))
		           LIMIT 1), 0)
		  FROM tickets t
		 WHERE t.order_id = $1 AND t.status = 'active'
		 ORDER BY t.created_at, t.id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines []RefundLine
	var weights []int64
	var totalWeight int64
	for rows.Next() {
		var l RefundLine
		var w int64
		if err := rows.Scan(&l.TicketID, &w); err != nil {
			return nil, err
		}
		lines = append(lines, l)
		weights = append(weights, w)
		totalWeight += w
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, nil
	}

	// Já estornado antes: o que resta a distribuir é o face menos o que já saiu.
	var already int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(face_cents),0) FROM refunds
		 WHERE order_id=$1 AND status IN ('pending','sent','confirmed')`, orderID).Scan(&already); err != nil {
		return nil, err
	}
	remaining := max(orderFace-already, 0)

	if totalWeight <= 0 { // sem preço utilizável: divide igual, sem perder centavo
		for i := range lines {
			weights[i] = 1
		}
		totalWeight = int64(len(lines))
	}
	var assigned int64
	for i := range lines {
		lines[i].FaceCents = mulDiv(remaining, weights[i], totalWeight)
		assigned += lines[i].FaceCents
	}
	// A sobra do arredondamento vai para os de maior peso, um centavo por vez.
	for rest := remaining - assigned; rest > 0; rest-- {
		best, bestW := 0, int64(-1)
		for i := range lines {
			if weights[i] > bestW {
				best, bestW = i, weights[i]
			}
		}
		lines[best].FaceCents++
		weights[best]-- // espalha em vez de empilhar tudo no mesmo ingresso
	}
	return lines, nil
}

// refundedConvenience é a conveniência já devolvida nesta ordem, para o estorno total
// devolver exatamente o que falta.
func refundedConvenience(ctx context.Context, tx pgx.Tx, orderID uuid.UUID) int64 {
	var v int64
	_ = tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(convenience_cents),0) FROM refunds
		 WHERE order_id=$1 AND status IN ('pending','sent','confirmed')`, orderID).Scan(&v)
	return v
}

func refundTicketIDs(ctx context.Context, tx pgx.Tx, refundID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `SELECT ticket_id FROM refund_tickets WHERE refund_id=$1`, refundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// rejectCheckedIn barra o estorno de ingresso que já teve admissão primária.
func rejectCheckedIn(ctx context.Context, tx pgx.Tx, lines []RefundLine) error {
	ids := make([]uuid.UUID, 0, len(lines))
	for _, l := range lines {
		ids = append(ids, l.TicketID)
	}
	var n int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM checkins WHERE ticket_id = ANY($1) AND NOT is_reentry`, ids).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrRefundCheckedIn
	}
	return nil
}

// mulDiv calcula v * num / den arredondando para o inteiro mais próximo, sem estourar em
// float: os valores são centavos e a precisão importa.
func mulDiv(v, num, den int64) int64 {
	if den == 0 {
		return 0
	}
	return (v*num + den/2) / den
}
