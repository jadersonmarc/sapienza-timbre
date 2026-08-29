package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/audit"
	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/nft"
	"github.com/jadersonmarc/sapienza-timbre/internal/notify"
	"github.com/jadersonmarc/sapienza-timbre/internal/transfer"
)

// ErrTicketCheckedIn barra a ação em ingresso que já entrou. Quem passou na portaria está lá
// dentro: reemitir cria um QR novo para quem já usou o antigo, e transferir muda o nome de
// alguém que já foi conferido na porta.
var errTicketCheckedIn = errors.New("ingresso com entrada registrada")

type ticketActionReq struct {
	// ToEmail, na reemissão, reenvia para outro endereço — o caso do e-mail digitado errado.
	// Na transferência, é o novo titular.
	ToEmail string `json:"to_email"`
	Reason  string `json:"reason"`
}

// producerReissue regera o ingresso e reenvia ao comprador.
func (s *Server) producerReissue(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	s.reissue(w, r, claims.ProducerID, audit.ActorProducer, claims.Subject, false)
}

// adminReissue passa por cima das guardas, com motivo obrigatório.
func (s *Server) adminReissue(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	producerID, err := uuid.Parse(r.PathValue("producerId"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "produtor inválido")
		return
	}
	s.reissue(w, r, producerID, audit.ActorAdmin, claims.Subject, true)
}

func (s *Server) reissue(w http.ResponseWriter, r *http.Request, producerID uuid.UUID,
	actorKind, actor string, override bool) {
	ticketID, body, ok := s.ticketAction(w, r, override)
	if !ok {
		return
	}

	var newID uuid.UUID
	var deliverTo, previousEmail string
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		if !override {
			if e := ensureNotCheckedIn(r.Context(), tx, ticketID); e != nil {
				return e
			}
		}
		var orderID *uuid.UUID
		if e := tx.QueryRow(r.Context(), `SELECT order_id FROM tickets WHERE id=$1`, ticketID).
			Scan(&orderID); e != nil {
			return e
		}
		if orderID != nil {
			if e := tx.QueryRow(r.Context(), `SELECT COALESCE(buyer_email,'') FROM orders WHERE id=$1`, *orderID).
				Scan(&previousEmail); e != nil {
				return e
			}
		}
		deliverTo = previousEmail

		// Endereço novo: o e-mail passa a ser o do pedido, e o anterior fica na trilha. Sem
		// gravar, o próximo reenvio voltaria para o endereço errado; sem guardar o antigo,
		// ninguém consegue explicar depois para onde o primeiro ingresso foi.
		if body.ToEmail != "" {
			to := normalizeEmail(body.ToEmail)
			if !looksLikeEmail(to) {
				return errBadEmail
			}
			if orderID != nil {
				if _, e := tx.Exec(r.Context(), `UPDATE orders SET buyer_email=$2, updated_at=now() WHERE id=$1`,
					*orderID, to); e != nil {
					return e
				}
			}
			deliverTo = to
		}

		// Reissue QUEIMA o ingresso anterior e emite outro na mesma venda: sem isso haveria
		// dois QR válidos para o mesmo lugar, que é fraude servida pelo painel.
		var e error
		newID, e = nft.Reissue(r.Context(), tx, s.signer, producerID, ticketID)
		if e != nil {
			return e
		}
		if e := s.emitter(producerID).EmitTickets(r.Context(), tx, []uuid.UUID{newID}, deliverTo); e != nil {
			return e
		}
		details := map[string]any{"novo_ingresso": newID.String()}
		if body.ToEmail != "" && previousEmail != "" {
			details["email_anterior"] = previousEmail
			details["email_novo"] = deliverTo
		}
		return audit.Append(r.Context(), tx, audit.Event{
			Entity: audit.EntityTicket, TicketID: &ticketID, ActorKind: actorKind, Actor: actor,
			FromStatus: "active", ToStatus: "reissued", Reason: body.Reason, Details: details,
		})
	}); err != nil {
		writeTicketActionErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ticket_id": newID, "delivered_to": deliverTo})
}

// producerTransfer troca o titular do ingresso pelo e-mail do novo dono.
//
// Reusa o caminho de custódia de plataforma do comprador — resolve o sujeito pelo e-mail,
// garante a carteira e chama a mesma transferência. Um segundo caminho para trocar o dono
// seria uma segunda chance de divergir das regras de teto e royalty.
func (s *Server) producerTransfer(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	s.transferByEmail(w, r, claims.ProducerID, audit.ActorProducer, claims.Subject, false)
}

func (s *Server) adminTransfer(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	producerID, err := uuid.Parse(r.PathValue("producerId"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "produtor inválido")
		return
	}
	s.transferByEmail(w, r, producerID, audit.ActorAdmin, claims.Subject, true)
}

func (s *Server) transferByEmail(w http.ResponseWriter, r *http.Request, producerID uuid.UUID,
	actorKind, actor string, override bool) {
	ticketID, body, ok := s.ticketAction(w, r, override)
	if !ok {
		return
	}
	to := normalizeEmail(body.ToEmail)
	if !looksLikeEmail(to) {
		writeErr(w, http.StatusBadRequest, "informe o e-mail do novo titular")
		return
	}

	var previousEmail, eventName string
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		if !override {
			if e := ensureNotCheckedIn(r.Context(), tx, ticketID); e != nil {
				return e
			}
		}
		_ = tx.QueryRow(r.Context(), `
			SELECT COALESCE(o.buyer_email,''), e.title FROM tickets t
			  JOIN events e ON e.id = t.event_id
			  LEFT JOIN orders o ON o.id = t.order_id
			 WHERE t.id=$1`, ticketID).Scan(&previousEmail, &eventName)

		toSubject, e := resolveSubjectByEmailTx(r.Context(), tx, to)
		if e != nil {
			return e
		}
		toWallet, e := resolveWalletTx(r.Context(), tx, toSubject)
		if e != nil {
			return e
		}
		// PriceCents zero: troca de titular não é revenda. Nada de cobrança, split ou royalty.
		if _, e := transfer.Execute(r.Context(), tx, ticketID, toWallet, 0); e != nil {
			return e
		}
		if _, e := tx.Exec(r.Context(), `
			UPDATE public.ticket_directory SET buyer_email=$2, subject_id=$3 WHERE ticket_id=$1`,
			ticketID, to, toSubject); e != nil {
			return e
		}
		// Os dois lados são avisados: quem perdeu o ingresso precisa saber por que ele sumiu
		// de "meus ingressos", e quem ganhou precisa do QR.
		s.notifyTransfer(r.Context(), tx, ticketID, eventName, previousEmail, to)
		return audit.Append(r.Context(), tx, audit.Event{
			Entity: audit.EntityTicket, TicketID: &ticketID, ActorKind: actorKind, Actor: actor,
			FromStatus: previousEmail, ToStatus: "transferred", Reason: body.Reason,
			Details: map[string]any{"titular_anterior": previousEmail, "titular_novo": to},
		})
	}); err != nil {
		writeTicketActionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "to_email": to})
}

// ticketHistory é a trilha do ingresso: emissões, transferências, entrada, estorno. É o que
// o produtor mostra quando o comprador contesta.
func (s *Server) ticketHistory(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	ticketID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "ingresso inválido")
		return
	}
	var events []audit.Event
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		events, e = audit.TicketHistory(r.Context(), tx, ticketID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// ── apoio ────────────────────────────────────────────────────────────────────

var errBadEmail = errors.New("e-mail inválido")

// ticketAction faz a parte comum: id, corpo e a exigência de motivo no override.
func (s *Server) ticketAction(w http.ResponseWriter, r *http.Request, override bool) (uuid.UUID, ticketActionReq, bool) {
	ticketID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "ingresso inválido")
		return uuid.Nil, ticketActionReq{}, false
	}
	// Corpo vazio é uso legítimo: reemitir para o mesmo endereço, sem motivo declarado. Só
	// o override do admin exige justificativa.
	var body ticketActionReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return uuid.Nil, ticketActionReq{}, false
	}
	if override && body.Reason == "" {
		writeErr(w, http.StatusBadRequest, "motivo obrigatório")
		return uuid.Nil, ticketActionReq{}, false
	}
	return ticketID, body, true
}

func ensureNotCheckedIn(ctx context.Context, tx pgx.Tx, ticketID uuid.UUID) error {
	var n int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM checkins WHERE ticket_id=$1 AND NOT is_reentry`, ticketID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errTicketCheckedIn
	}
	return nil
}

func (s *Server) notifyTransfer(ctx context.Context, tx pgx.Tx, ticketID uuid.UUID, eventName, from, to string) {
	if s.seams.Notify == nil || from == "" || from == to {
		return
	}
	_ = s.seams.Notify.Send(ctx, tx, notify.Message{
		Kind: notify.KindWaitlist, To: from, EventName: eventName,
		Subject: "Seu ingresso foi transferido",
		Body: "O ingresso para " + eventName + " foi transferido para outra pessoa pelo produtor." +
			" Ele não aparece mais em seus ingressos.",
		// A chave amarra o aviso ao ingresso: reprocessar o caminho não avisa duas vezes.
		IdempotencyKey: "transfer_out:" + ticketID.String() + ":" + from,
	})
}

func writeTicketActionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errTicketCheckedIn):
		writeErr(w, http.StatusConflict, "este ingresso já teve entrada registrada")
	case errors.Is(err, errBadEmail):
		writeErr(w, http.StatusBadRequest, "e-mail inválido")
	case errors.Is(err, nft.ErrNotReissuable):
		writeErr(w, http.StatusConflict, "ingresso não reemitível (já estornado, transferido ou reemitido)")
	case errors.Is(err, transfer.ErrNotTransferable):
		writeErr(w, http.StatusConflict, "ingresso não transferível")
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}
