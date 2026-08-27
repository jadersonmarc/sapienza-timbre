package checkout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Quem pediu.
const (
	RequesterBuyer    = "buyer"
	RequesterProducer = "producer"
	RequesterAdmin    = "admin"
)

// Trilhas de autorização. A trilha é DERIVADA da política no momento do pedido, nunca
// escolhida por quem pede — senão o comprador se auto-concederia o caminho automático.
const (
	// TrackWithdrawal é o arrependimento dentro da janela: direito, não favor. Aprovado na
	// hora, sem passar pelo produtor.
	TrackWithdrawal = "withdrawal"
	// TrackDiscretionary é o pedido fora da janela: liberalidade, decidida pelo produtor.
	TrackDiscretionary = "discretionary"
	// TrackProducerInitiated é o produtor cancelando, sem pedido do comprador.
	TrackProducerInitiated = "producer_initiated"
	// TrackAdminOverride é a plataforma por cima de tudo, com motivo obrigatório.
	TrackAdminOverride = "admin_override"
)

// Estados do pedido.
const (
	ReqPending    = "pending"
	ReqApproved   = "approved"
	ReqRejected   = "rejected"
	ReqProcessing = "processing"
	ReqCompleted  = "completed"
	ReqFailed     = "failed"
)

var (
	// ErrRequestOpen é pedido vivo repetido para a mesma ordem. Barrado por índice único.
	ErrRequestOpen = errors.New("já existe um pedido de estorno em aberto para esta compra")
	// ErrDiscretionaryClosed é o produtor que não aceita analisar pedido fora da janela.
	// Recusar na hora é melhor que deixar parado numa fila que ninguém vai olhar.
	ErrDiscretionaryClosed = errors.New("fora da janela de arrependimento e o produtor não aceita pedidos por liberalidade")
	// ErrRequestNotOpen é decidir um pedido que já foi decidido.
	ErrRequestNotOpen = errors.New("pedido já decidido")
)

// RefundRequest é o pedido de estorno, com o que a decisão precisa.
type RefundRequest struct {
	ID             uuid.UUID   `json:"id"`
	OrderID        uuid.UUID   `json:"order_id"`
	TicketIDs      []uuid.UUID `json:"ticket_ids"`
	RequestedBy    string      `json:"requested_by"`
	Track          string      `json:"track"`
	Status         string      `json:"status"`
	Reason         *string     `json:"reason"`
	DecidedBy      *string     `json:"decided_by"`
	DecidedAt      *time.Time  `json:"decided_at"`
	DecisionReason *string     `json:"decision_reason"`
	RespondsBy     *time.Time  `json:"responds_by"`
	RefundID       *uuid.UUID  `json:"refund_id"`
	AmountCents    int64       `json:"refund_amount_cents"`
	CreatedAt      time.Time   `json:"created_at"`
	// Overdue marca o pedido de liberalidade que passou do prazo de resposta. Não aprova
	// nada — só diz que o produtor está devendo resposta.
	Overdue bool `json:"overdue"`
}

// NewRefundRequest descreve um pedido a abrir.
type NewRefundRequest struct {
	OrderID   uuid.UUID
	TicketIDs []uuid.UUID
	// RequestedBy define a trilha junto com a política: comprador é classificado pela
	// janela; produtor e admin têm trilha própria.
	RequestedBy string
	SubjectID   *uuid.UUID
	Reason      string
	Actor       string
}

// CreateRequest abre o pedido e resolve a trilha pela política vigente. Devolve o pedido
// com o status já correto: arrependimento nasce aprovado (é direito), liberalidade nasce
// pendente com prazo de resposta, e as trilhas de produtor/admin nascem aprovadas porque
// quem pediu já é quem decide.
func CreateRequest(ctx context.Context, tx pgx.Tx, in NewRefundRequest) (RefundRequest, error) {
	var r RefundRequest

	var eventID uuid.UUID
	var orderStatus string
	var boughtAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT event_id, status, created_at FROM orders WHERE id=$1`, in.OrderID).
		Scan(&eventID, &orderStatus, &boughtAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return r, ErrRefundNothing
		}
		return r, err
	}
	if orderStatus == "refunded" {
		return r, ErrRefundNothing
	}
	if orderStatus != "paid" && orderStatus != "partially_refunded" {
		return r, ErrRefundNotPaid
	}

	policy, err := ResolvePolicy(ctx, tx, eventID)
	if err != nil {
		return r, err
	}
	var startsAt *time.Time
	_ = tx.QueryRow(ctx, `SELECT starts_at FROM events WHERE id=$1`, eventID).Scan(&startsAt)

	track, status := TrackProducerInitiated, ReqApproved
	var respondsBy *time.Time
	switch in.RequestedBy {
	case RequesterAdmin:
		track = TrackAdminOverride
	case RequesterProducer:
		track = TrackProducerInitiated
	case RequesterBuyer:
		if ok, _ := policy.WithinWithdrawal(boughtAt, startsAt, time.Now()); ok {
			// Direito de arrependimento: não passa pelo produtor.
			track, status = TrackWithdrawal, ReqApproved
		} else {
			if !policy.ProducerDiscretionaryEnabled {
				return r, ErrDiscretionaryClosed
			}
			track, status = TrackDiscretionary, ReqPending
			d := time.Now().Add(time.Duration(policy.DiscretionaryResponseHours) * time.Hour)
			respondsBy = &d
		}
	default:
		return r, fmt.Errorf("%w: solicitante inválido: %q", ErrBadRequest, in.RequestedBy)
	}

	ids := in.TicketIDs
	if ids == nil {
		ids = []uuid.UUID{}
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO refund_requests (order_id, ticket_ids, requested_by, requested_subject,
		                             track, status, reason, responds_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id, created_at`,
		in.OrderID, ids, in.RequestedBy, in.SubjectID, track, status,
		nilIfEmpty(in.Reason), respondsBy).Scan(&r.ID, &r.CreatedAt); err != nil {
		if isUniqueViolation(err) {
			return r, ErrRequestOpen
		}
		return r, fmt.Errorf("abrir pedido de estorno: %w", err)
	}
	r.OrderID, r.TicketIDs, r.RequestedBy = in.OrderID, ids, in.RequestedBy
	r.Track, r.Status, r.RespondsBy = track, status, respondsBy

	actorKind := in.RequestedBy
	if err := logRequest(ctx, tx, r.ID, actorKind, in.Actor, "", status, in.Reason); err != nil {
		return r, err
	}
	return r, nil
}

// DecideRequest aprova ou recusa um pedido pendente. A recusa EXIGE motivo: recusar sem
// dizer por quê é o que faz o comprador voltar pelo canal mais caro.
func DecideRequest(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, approve bool,
	actorKind, actor, reason string) (RefundRequest, error) {
	var r RefundRequest
	var status string
	if err := tx.QueryRow(ctx, `
		SELECT id, order_id, ticket_ids, track, status FROM refund_requests
		 WHERE id=$1 FOR UPDATE`, requestID).
		Scan(&r.ID, &r.OrderID, &r.TicketIDs, &r.Track, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return r, ErrRefundNothing
		}
		return r, err
	}
	if status != ReqPending {
		return r, ErrRequestNotOpen
	}
	if !approve && reason == "" {
		return r, fmt.Errorf("%w: recusa exige motivo", ErrBadRequest)
	}

	to := ReqRejected
	if approve {
		to = ReqApproved
	}
	if _, err := tx.Exec(ctx, `
		UPDATE refund_requests SET status=$2, decided_by=$3, decided_at=now(),
		       decision_reason=$4, updated_at=now()
		 WHERE id=$1`, requestID, to, actorKind+":"+actor, nilIfEmpty(reason)); err != nil {
		return r, err
	}
	r.Status = to
	return r, logRequest(ctx, tx, requestID, actorKind, actor, status, to, reason)
}

// MarkRequestProcessing prende o pedido antes da chamada ao gateway, para uma segunda
// aprovação não disparar um segundo estorno enquanto o primeiro está em voo.
func MarkRequestProcessing(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `
		UPDATE refund_requests SET status=$2, updated_at=now()
		 WHERE id=$1 AND status=$3`, requestID, ReqProcessing, ReqApproved)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrRequestNotOpen
	}
	return logRequest(ctx, tx, requestID, "system", "", ReqApproved, ReqProcessing, "")
}

// CompleteRequest amarra o pedido à execução que o atendeu.
func CompleteRequest(ctx context.Context, tx pgx.Tx, requestID, refundID uuid.UUID,
	face, convenience, total int64) error {
	if _, err := tx.Exec(ctx, `
		UPDATE refund_requests SET status=$2, refund_id=$3, face_cents=$4,
		       convenience_cents=$5, refund_amount_cents=$6, updated_at=now()
		 WHERE id=$1`, requestID, ReqCompleted, refundID, face, convenience, total); err != nil {
		return err
	}
	return logRequest(ctx, tx, requestID, "system", "", ReqProcessing, ReqCompleted, "")
}

// FailRequest registra que a execução não foi adiante. O pedido SAI do estado vivo, e é
// isso que importa: o índice único guarda um pedido vivo por compra, então uma tentativa
// barrada que ficasse viva trancaria a ordem para todo mundo — inclusive para o admin que
// vem justamente para passar por cima da guarda que barrou.
//
// A tentativa e o motivo dela ficam registrados: é para isso que a auditoria existe.
func FailRequest(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, cause string) error {
	var from string
	if err := tx.QueryRow(ctx, `
		UPDATE refund_requests SET status=$2, error=$3, updated_at=now()
		 WHERE id=$1 RETURNING (SELECT status FROM refund_requests WHERE id=$1)`,
		requestID, ReqFailed, cause).Scan(&from); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	return logRequest(ctx, tx, requestID, "system", "", from, ReqFailed, cause)
}

// ListRequests devolve os pedidos do produtor, opcionalmente filtrados por status.
func ListRequests(ctx context.Context, tx pgx.Tx, status string) ([]RefundRequest, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, order_id, ticket_ids, requested_by, track, status, reason, decided_by,
		       decided_at, decision_reason, responds_by, refund_id, refund_amount_cents, created_at
		  FROM refund_requests
		 WHERE ($1 = '' OR status = $1)
		 ORDER BY created_at DESC`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RefundRequest
	now := time.Now()
	for rows.Next() {
		var r RefundRequest
		if err := rows.Scan(&r.ID, &r.OrderID, &r.TicketIDs, &r.RequestedBy, &r.Track, &r.Status,
			&r.Reason, &r.DecidedBy, &r.DecidedAt, &r.DecisionReason, &r.RespondsBy,
			&r.RefundID, &r.AmountCents, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Overdue = r.Status == ReqPending && r.RespondsBy != nil && now.After(*r.RespondsBy)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RequestEvent é uma linha da trilha de auditoria.
type RequestEvent struct {
	At         time.Time `json:"at"`
	ActorKind  string    `json:"actor_kind"`
	Actor      *string   `json:"actor"`
	FromStatus *string   `json:"from_status"`
	ToStatus   string    `json:"to_status"`
	Reason     *string   `json:"reason"`
}

// RequestHistory devolve a trilha completa de um pedido, em ordem.
func RequestHistory(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) ([]RequestEvent, error) {
	rows, err := tx.Query(ctx, `
		SELECT at, actor_kind, actor, from_status, to_status, reason
		  FROM refund_request_events WHERE request_id=$1 ORDER BY at, id`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RequestEvent
	for rows.Next() {
		var e RequestEvent
		if err := rows.Scan(&e.At, &e.ActorKind, &e.Actor, &e.FromStatus, &e.ToStatus, &e.Reason); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// logRequest grava a transição. Append-only: decisão tomada não se edita, e uma decisão
// que muda vira outra linha.
func logRequest(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, actorKind, actor, from, to, reason string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO refund_request_events (request_id, actor_kind, actor, from_status, to_status, reason)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		requestID, actorKind, nilIfEmpty(actor), nilIfEmpty(from), to, nilIfEmpty(reason))
	return err
}
