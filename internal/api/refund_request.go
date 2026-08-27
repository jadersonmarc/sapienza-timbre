package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/checkout"
	"github.com/jadersonmarc/sapienza-timbre/internal/notify"
)

// Apelidos locais para os avisos do ciclo do pedido, só para a leitura das chamadas.
const (
	kindRefundRequested = notify.KindRefundRequested
	kindRefundRejected  = notify.KindRefundRejected
)

// buyerRefundRequest é o comprador pedindo o dinheiro de volta.
//
// A trilha NÃO é escolhida por ele: sai da política do evento. Dentro da janela de
// arrependimento é direito e o estorno acontece na hora, sem passar pelo produtor; fora
// dela é liberalidade e vai para a fila da casa. Deixar quem pede escolher o caminho seria
// deixar o comprador se autoconceder o automático.
func (s *Server) buyerRefundRequest(w http.ResponseWriter, r *http.Request, subjectID uuid.UUID) {
	orderID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "pedido inválido")
		return
	}
	var body refundReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	tickets, err := parseTicketIDs(body.TicketIDs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// A ordem é do comprador? O índice público responde sem varrer schema, e é ele que
	// impede alguém de pedir o estorno da compra de outra pessoa.
	var producerID uuid.UUID
	if err := s.pool.QueryRow(r.Context(), `
		SELECT producer_id FROM order_directory WHERE order_id=$1 AND subject_id=$2`,
		orderID, subjectID).Scan(&producerID); err != nil {
		writeErr(w, http.StatusNotFound, "compra não encontrada")
		return
	}

	var request checkout.RefundRequest
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		var e error
		request, e = checkout.CreateRequest(r.Context(), tx, checkout.NewRefundRequest{
			OrderID: orderID, TicketIDs: tickets, RequestedBy: checkout.RequesterBuyer,
			SubjectID: &subjectID, Reason: body.Reason, Actor: subjectID.String(),
		})
		return e
	}); err != nil {
		writeRefundErr(w, err)
		return
	}

	// Arrependimento é direito: executa agora. Liberalidade fica pendente — e silêncio do
	// produtor não aprova nada, por desenho.
	if request.Track == checkout.TrackWithdrawal {
		out, err := s.executeRequest(r.Context(), producerID, request)
		if err != nil {
			writeRefundErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"request": request, "refund": out})
		return
	}
	// Pedido que some sem aviso vira ligação, e ligação vira contestação.
	s.notifyRequest(r.Context(), producerID, request, kindRefundRequested, "")
	writeJSON(w, http.StatusAccepted, map[string]any{"request": request})
}

// notifyRequest avisa o comprador sobre o andamento do pedido. Best effort e fora do
// caminho da decisão: falhar o e-mail não pode desfazer o que já foi decidido.
func (s *Server) notifyRequest(ctx context.Context, producerID uuid.UUID, req checkout.RefundRequest, kind, reason string) {
	if s.seams.Notify == nil {
		return
	}
	if err := s.withTenant(ctx, producerID, func(tx pgx.Tx) error {
		var to, eventName string
		if e := tx.QueryRow(ctx, `
			SELECT COALESCE(o.buyer_email,''), e.title FROM orders o
			  JOIN events e ON e.id = o.event_id WHERE o.id=$1`, req.OrderID).Scan(&to, &eventName); e != nil {
			return e
		}
		if to == "" {
			return nil
		}
		msg := notify.Message{
			Kind: kind, To: to, EventName: eventName, OrderID: &req.OrderID,
			// A chave é o pedido e o momento dele: "recebemos" e "recusado" são avisos
			// diferentes do mesmo pedido, e os dois precisam sair.
			IdempotencyKey: kind + ":" + req.ID.String(),
		}
		if req.RespondsBy != nil {
			msg.RespondsBy = req.RespondsBy.Format("02/01/2006")
		}
		msg.DecisionReason = reason
		return s.seams.Notify.Send(ctx, tx, msg)
	}); err != nil {
		slog.Warn("avisar comprador sobre o pedido de estorno", "pedido", req.ID, "err", err)
	}
}

// myRefundRequests lista os pedidos do comprador, para ele acompanhar sem ligar para a casa.
func (s *Server) myRefundRequests(w http.ResponseWriter, r *http.Request, subjectID uuid.UUID) {
	// Os pedidos vivem no schema de cada produtor, e uma pessoa compra de vários: o índice
	// público de pedidos é o que diz onde procurar.
	rows, err := s.pool.Query(r.Context(), `
		SELECT DISTINCT producer_id FROM order_directory WHERE subject_id=$1`, subjectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var producers []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		producers = append(producers, id)
	}
	rows.Close()

	out := []checkout.RefundRequest{}
	for _, pid := range producers {
		var list []checkout.RefundRequest
		if err := s.withTenant(r.Context(), pid, func(tx pgx.Tx) error {
			return scanBuyerRequests(r.Context(), tx, subjectID, &list)
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, list...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": out})
}

func scanBuyerRequests(ctx context.Context, tx pgx.Tx, subjectID uuid.UUID, out *[]checkout.RefundRequest) error {
	rows, err := tx.Query(ctx, `
		SELECT id, order_id, ticket_ids, requested_by, track, status, reason, decided_by,
		       decided_at, decision_reason, responds_by, refund_id, refund_amount_cents, created_at
		  FROM refund_requests WHERE requested_subject=$1 ORDER BY created_at DESC`, subjectID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var q checkout.RefundRequest
		if err := rows.Scan(&q.ID, &q.OrderID, &q.TicketIDs, &q.RequestedBy, &q.Track, &q.Status,
			&q.Reason, &q.DecidedBy, &q.DecidedAt, &q.DecisionReason, &q.RespondsBy,
			&q.RefundID, &q.AmountCents, &q.CreatedAt); err != nil {
			return err
		}
		*out = append(*out, q)
	}
	return rows.Err()
}

// listRefundRequests é a fila do produtor. Sem filtro, mostra tudo; `?status=pending` é o
// que o painel abre por padrão.
func (s *Server) listRefundRequests(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	status := r.URL.Query().Get("status")
	var list []checkout.RefundRequest
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		list, e = checkout.ListRequests(r.Context(), tx, status)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []checkout.RefundRequest{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": list})
}

// refundRequestHistory devolve a trilha de auditoria de um pedido — o que o produtor mostra
// quando o comprador reclama, e o que a plataforma mostra quando o produtor reclama.
func (s *Server) refundRequestHistory(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "pedido inválido")
		return
	}
	var events []checkout.RequestEvent
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		events, e = checkout.RequestHistory(r.Context(), tx, id)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if events == nil {
		events = []checkout.RequestEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

type decisionReq struct {
	Reason string `json:"reason"`
}

// decideRefundRequest é o produtor aprovando ou recusando um pedido de liberalidade.
// Aprovar executa o estorno na sequência; recusar exige motivo.
func (s *Server) decideRefundRequest(approve bool) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
		s.decide(w, r, claims.ProducerID, approve, "producer", claims.Subject)
	}
}

// adminDecideRefundRequest é a plataforma decidindo por cima do produtor. Mesmo caminho, com
// o ator registrado como admin — a diferença precisa aparecer na auditoria.
func (s *Server) adminDecideRefundRequest(approve bool) adminHandler {
	return func(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
		producerID, err := uuid.Parse(r.PathValue("producerId"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "produtor inválido")
			return
		}
		s.decide(w, r, producerID, approve, "admin", claims.Subject)
	}
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request, producerID uuid.UUID,
	approve bool, actorKind, actor string) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "pedido inválido")
		return
	}
	var body decisionReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}

	var request checkout.RefundRequest
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		var e error
		request, e = checkout.DecideRequest(r.Context(), tx, id, approve, actorKind, actor, body.Reason)
		return e
	}); err != nil {
		writeRefundErr(w, err)
		return
	}
	if !approve {
		// A recusa vai COM o motivo: recusa sem explicação é a que volta pelo canal caro.
		s.notifyRequest(r.Context(), producerID, request, kindRefundRejected, body.Reason)
		writeJSON(w, http.StatusOK, map[string]any{"request": request})
		return
	}

	out, err := s.executeRequest(r.Context(), producerID, request)
	if err != nil {
		writeRefundErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"request": request, "refund": out})
}

// executeRequest roda o estorno que um pedido aprovado autoriza. O admin decidindo passa por
// cima das guardas; o produtor, não.
func (s *Server) executeRequest(ctx context.Context, producerID uuid.UUID, req checkout.RefundRequest) (refundResp, error) {
	initiatedBy := checkout.RefundByProducer
	if req.Track == checkout.TrackAdminOverride {
		initiatedBy = checkout.RefundByAdmin
	}
	reason := ""
	if req.Reason != nil {
		reason = *req.Reason
	}
	out, err := s.runRefund(ctx, producerID, checkout.RefundInput{
		OrderID:        req.OrderID,
		TicketIDs:      req.TicketIDs,
		InitiatedBy:    initiatedBy,
		Reason:         reason,
		AllowCheckedIn: req.Track == checkout.TrackAdminOverride,
	}, &req.ID)
	if err != nil {
		return refundResp{}, err
	}
	out.RequestID = &req.ID
	return out, nil
}

func parseTicketIDs(raw []string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(raw))
	for _, s := range raw {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, errors.New("ingresso inválido: " + s)
		}
		out = append(out, id)
	}
	return out, nil
}
