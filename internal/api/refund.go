package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/attest"
	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/checkout"
	"github.com/jadersonmarc/sapienza-timbre/internal/notify"
	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
)

type refundReq struct {
	// TicketIDs vazio = ordem inteira.
	TicketIDs []string `json:"ticket_ids"`
	Reason    string   `json:"reason"`
}

type refundResp struct {
	ID               uuid.UUID `json:"id"`
	Scope            string    `json:"scope"`
	Tickets          int       `json:"tickets"`
	FaceCents        int64     `json:"face_cents"`
	ConvenienceCents int64     `json:"convenience_cents"`
	TotalCents       int64     `json:"total_cents"`
	// CoveredByPlatform é o caso em que a subconta do produtor não cobriu: o comprador foi
	// estornado assim mesmo e o produtor ficou devendo.
	CoveredByPlatform bool       `json:"covered_by_platform"`
	RequestID         *uuid.UUID `json:"request_id,omitempty"`
}

// producerRefund estorna um pedido (owner do produtor). Guarda de entrada registrada vale:
// quem já entrou consumiu o serviço, e devolver aí é decisão da plataforma.
func (s *Server) producerRefund(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	s.handleRefund(w, r, claims.ProducerID, checkout.RefundByProducer, false)
}

// adminRefund estorna um pedido de qualquer produtor, passando por cima das guardas. O
// motivo é obrigatório: é a linha que explica, meses depois, por que este pedido foi
// tratado fora da regra.
func (s *Server) adminRefund(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	producerID, err := uuid.Parse(r.PathValue("producerId"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "produtor inválido")
		return
	}
	s.handleRefund(w, r, producerID, checkout.RefundByAdmin, true)
}

func (s *Server) handleRefund(w http.ResponseWriter, r *http.Request, producerID uuid.UUID, by string, override bool) {
	orderID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "pedido inválido")
		return
	}
	// Corpo vazio é estorno TOTAL sem motivo declarado — só o admin precisa justificar.
	var req refundReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if override && req.Reason == "" {
		writeErr(w, http.StatusBadRequest, "motivo obrigatório")
		return
	}
	tickets := make([]uuid.UUID, 0, len(req.TicketIDs))
	for _, raw := range req.TicketIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "ingresso inválido: "+raw)
			return
		}
		tickets = append(tickets, id)
	}

	// Mesmo o cancelamento por conta própria abre um pedido: é o registro que responde
	// "quem mandou estornar isso, e por quê" meses depois. Sem ele, a trilha de auditoria
	// teria buraco justo nas decisões que ninguém pediu.
	requester := checkout.RequesterProducer
	actorKind := "producer"
	if override {
		requester, actorKind = checkout.RequesterAdmin, "admin"
	}
	var request checkout.RefundRequest
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		var e error
		request, e = checkout.CreateRequest(r.Context(), tx, checkout.NewRefundRequest{
			OrderID: orderID, TicketIDs: tickets, RequestedBy: requester,
			Reason: req.Reason, Actor: actorKind,
		})
		return e
	}); err != nil {
		writeRefundErr(w, err)
		return
	}

	out, err := s.runRefund(r.Context(), producerID, checkout.RefundInput{
		OrderID: orderID, TicketIDs: tickets, InitiatedBy: by,
		Reason: req.Reason, AllowCheckedIn: override,
	}, &request.ID)
	if err != nil {
		writeRefundErr(w, err)
		return
	}
	out.RequestID = &request.ID
	writeJSON(w, http.StatusOK, out)
}

// runRefund executa o estorno em DUAS FASES, e é essa separação que o torna seguro.
//
// A chamada ao gateway não pode viver dentro da transação: se o dinheiro volta e a
// transação faz rollback, o comprador foi estornado e o ingresso continua válido, sem
// registro nenhum. Então a intenção é gravada e COMMITADA antes de qualquer chamada
// externa, e os efeitos entram depois, numa segunda transação. Falhar no meio deixa a
// operação marcada 'failed', com os ingressos soltos e nada aplicado pela metade.
func (s *Server) runRefund(ctx context.Context, producerID uuid.UUID, in checkout.RefundInput,
	requestID *uuid.UUID) (refundResp, error) {
	var prepared checkout.PreparedRefund
	if err := s.withTenant(ctx, producerID, func(tx pgx.Tx) error {
		// O pedido vai a 'processing' na MESMA transação que reserva os ingressos: uma
		// segunda aprovação clicada enquanto a primeira está em voo não pode virar um
		// segundo estorno.
		if requestID != nil {
			if e := checkout.MarkRequestProcessing(ctx, tx, *requestID); e != nil {
				return e
			}
		}
		var e error
		prepared, e = checkout.PrepareRefund(ctx, tx, in)
		return e
	}); err != nil {
		// A tentativa barrada não pode ficar viva: o índice único guarda um pedido vivo por
		// compra, e um pedido travado aqui trancaria a ordem — inclusive para o admin que
		// vem justamente para passar por cima da guarda que barrou.
		s.failRequest(ctx, producerID, requestID, err.Error())
		return refundResp{}, err
	}

	// ── fase 2: fora de transação ────────────────────────────────────────────
	var covered bool
	refund, err := s.seams.Payment.Refund(ctx, payment.RefundRequest{
		AsaasRef:   prepared.AsaasRef,
		ValueCents: prepared.TotalCents,
		// Carrega a nossa chave de idempotência: o gateway não tem cabeçalho próprio.
		Description: "timbre:refund:" + prepared.ID.String(),
	})
	switch {
	case err == nil, errors.Is(err, payment.ErrRefundAlreadyExists):
		// Estorno repetido é sucesso disfarçado: o dinheiro já voltou.
	case errors.Is(err, payment.ErrRefundInsufficientFunds):
		// A subconta do produtor não cobre. O comprador é estornado assim mesmo — a
		// plataforma cobre e o produtor fica devendo, e a dívida sai dos próximos repasses.
		// Deixar o comprador sem o dinheiro porque o produtor sacou não é opção.
		covered = true
		slog.Warn("estorno coberto pela plataforma", "refund", prepared.ID, "produtor", producerID,
			"valor", prepared.TotalCents)
	default:
		cause := err.Error()
		if e := s.withTenant(ctx, producerID, func(tx pgx.Tx) error {
			if requestID != nil {
				if e := checkout.FailRequest(ctx, tx, *requestID, cause); e != nil {
					return e
				}
			}
			return checkout.FailRefund(ctx, tx, prepared.ID, cause)
		}); e != nil {
			slog.Error("marcar estorno falho", "refund", prepared.ID, "err", e)
		}
		return refundResp{}, fmt.Errorf("estornar no gateway: %w", err)
	}

	// ── fase 3: efeitos ──────────────────────────────────────────────────────
	if err := s.withTenant(ctx, producerID, func(tx pgx.Tx) error {
		if e := checkout.MarkRefundSent(ctx, tx, prepared.ID, refund.ID); e != nil {
			return e
		}
		if e := checkout.CompleteRefund(ctx, tx, prepared.ID, covered); e != nil {
			return e
		}
		if requestID != nil {
			if e := checkout.CompleteRequest(ctx, tx, *requestID, prepared.ID,
				prepared.FaceCents, prepared.ConvenienceCents, prepared.TotalCents); e != nil {
				return e
			}
		}
		return notifyRefundOrder(ctx, s.seams.Notify, tx, prepared.OrderID, prepared.TotalCents)
	}); err != nil {
		s.failRequest(ctx, producerID, requestID, err.Error())
		return refundResp{}, err
	}

	// Evento já fechado: o registro canônico passou a mentir e precisa ser republicado. A
	// versão nova aponta para a anterior (supersedes_id) e a anterior continua acessível —
	// correção de atestado é versão nova, nunca edição.
	s.recloseIfClosed(ctx, producerID, prepared.OrderID)

	return refundResp{
		ID: prepared.ID, Scope: prepared.Scope, Tickets: len(prepared.Lines),
		FaceCents: prepared.FaceCents, ConvenienceCents: prepared.ConvenienceCents,
		TotalCents: prepared.TotalCents, CoveredByPlatform: covered,
	}, nil
}

// failRequest tira o pedido do estado vivo depois de uma tentativa que não foi adiante.
// Best effort: o erro que trouxe até aqui é o que interessa ao chamador, e engolir este
// segundo erro é melhor que trocá-lo pelo primeiro — mas ele fica no log.
func (s *Server) failRequest(ctx context.Context, producerID uuid.UUID, requestID *uuid.UUID, cause string) {
	if requestID == nil {
		return
	}
	if err := s.withTenant(ctx, producerID, func(tx pgx.Tx) error {
		return checkout.FailRequest(ctx, tx, *requestID, cause)
	}); err != nil {
		slog.Error("encerrar pedido de estorno após falha", "pedido", *requestID, "err", err)
	}
}

// recloseIfClosed republica o atestado quando o evento do pedido já estava fechado. Best
// effort e fora do caminho do dinheiro: o estorno já aconteceu, e falhar aqui não pode
// desfazê-lo — mas fica no log, porque um atestado desatualizado é comprovação errada.
func (s *Server) recloseIfClosed(ctx context.Context, producerID, orderID uuid.UUID) {
	if err := s.withTenant(ctx, producerID, func(tx pgx.Tx) error {
		var eventID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT event_id FROM orders WHERE id=$1`, orderID).Scan(&eventID); err != nil {
			return err
		}
		// Um lote de cancelamento em curso é dono da republicação: ele republica UMA vez no
		// fim. Republicar aqui também daria uma versão por pedido devolvido.
		stale, err := checkout.IsAttestationStale(ctx, tx, eventID)
		if err != nil || stale {
			return err
		}
		cur, err := attest.Current(ctx, tx, eventID)
		if err != nil || cur == nil {
			return err
		}
		_, err = attest.Close(ctx, tx, s.attest, s.anchorer, s.anchorMode, s.attestKeyID, producerID, eventID)
		return err
	}); err != nil {
		slog.Error("republicar atestado após estorno", "pedido", orderID, "err", err)
	}
}

// notifyRefundOrder avisa o comprador. Assíncrono — nunca bloqueia o estorno.
func notifyRefundOrder(ctx context.Context, n notify.Notifier, tx pgx.Tx, orderID uuid.UUID, valueCents int64) error {
	if n == nil {
		return nil
	}
	var to, eventName string
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(o.buyer_email,''), e.title FROM orders o
		  JOIN events e ON e.id = o.event_id WHERE o.id = $1`, orderID).Scan(&to, &eventName)
	if errors.Is(err, pgx.ErrNoRows) || to == "" {
		return nil
	}
	if err != nil {
		return err
	}
	return n.Send(ctx, notify.Message{
		Kind: notify.KindRefunded, To: to, EventName: eventName,
		OrderValueCents: valueCents, OrderID: &orderID,
	})
}

func writeRefundErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, checkout.ErrRefundNothing):
		writeErr(w, http.StatusNotFound, "nada a estornar neste pedido")
	case errors.Is(err, checkout.ErrRefundNotPaid):
		writeErr(w, http.StatusConflict, "pedido não pago")
	case errors.Is(err, checkout.ErrRefundInFlight):
		writeErr(w, http.StatusConflict, "já existe um estorno em andamento para este ingresso")
	case errors.Is(err, checkout.ErrRefundCheckedIn):
		writeErr(w, http.StatusConflict, "ingresso com entrada registrada não é estornável")
	case errors.Is(err, checkout.ErrRequestOpen):
		writeErr(w, http.StatusConflict, "já existe um pedido de estorno em aberto para esta compra")
	case errors.Is(err, checkout.ErrRequestNotOpen):
		writeErr(w, http.StatusConflict, "este pedido já foi decidido")
	case errors.Is(err, checkout.ErrDiscretionaryClosed):
		writeErr(w, http.StatusConflict, "fora da janela de arrependimento, e esta casa não aceita pedidos por liberalidade")
	case errors.Is(err, checkout.ErrBadRequest):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}
