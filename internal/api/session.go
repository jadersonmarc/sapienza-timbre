package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/checkout"
	"github.com/jadersonmarc/sapienza-timbre/internal/store"
)

// sessionPublic serializa a sessão para o cliente (inclui o anon_token — é a chave da
// sessão antes do acesso).
func sessionPublic(s checkout.Session) map[string]any {
	return map[string]any{
		"id": s.ID, "event_id": s.EventID, "anon_token": s.AnonToken,
		"status": s.Status, "expires_at": s.ExpiresAt,
		"items": map[string]any{
			"lot_id": s.Items.LotID, "quantity": s.Items.Quantity,
			"seat_ids": s.Items.SeatIDs, "half_price_qty": s.Items.HalfPriceQty,
			"coupon_code": s.Items.CouponCode,
			// A ficha volta para o cliente reidratar o formulário: quem recarregou a
			// página no meio do preenchimento não redigita tudo.
			"attendees": s.Items.Attendees,
		},
	}
}

// createSessionReq é a seleção + o anon_token do navegador (identifica sessões pré-acesso).
type createSessionReq struct {
	EventID      uuid.UUID   `json:"event_id"`
	Quantity     int         `json:"quantity"`
	SeatIDs      []uuid.UUID `json:"seat_ids"`
	HalfPriceQty int         `json:"half_price_qty"`
	CouponCode   string      `json:"coupon_code"`
	CampaignID   *uuid.UUID  `json:"campaign_id"`
	AnonToken    string      `json:"anon_token"`
}

// createSession cria a sessão e reserva (lote + assentos). Sem auth. Aplica teto por IP
// (sessões e assentos) contra abuso; registra em log quando um limite dispara.
func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var body createSessionReq
	if err := decode(w, r, &body); err != nil || body.EventID == uuid.Nil {
		writeErr(w, http.StatusBadRequest, "event_id obrigatório")
		return
	}
	producerID, err := s.producerOfEvent(r.Context(), body.EventID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "evento não encontrado ou não publicado")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ip := clientIP(r)
	req := checkout.Request{
		EventID: body.EventID, Quantity: body.Quantity, SeatIDs: body.SeatIDs,
		HalfPriceQty: body.HalfPriceQty, CouponCode: body.CouponCode, CampaignID: body.CampaignID,
	}
	var sess checkout.Session
	err = s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		var e error
		sess, e = checkout.CreateSession(r.Context(), tx, req, body.AnonToken, ip, s.checkoutLimits)
		if e != nil {
			return e
		}
		_, e = tx.Exec(r.Context(), `
			INSERT INTO public.checkout_session_index (session_id, producer_id, event_id)
			VALUES ($1,$2,$3) ON CONFLICT (session_id) DO NOTHING`, sess.ID, producerID, sess.EventID)
		return e
	})
	if err != nil {
		switch {
		case errors.Is(err, checkout.ErrTooManySessions), errors.Is(err, checkout.ErrTooManySeats):
			writeErr(w, http.StatusTooManyRequests, "muitas reservas deste IP no momento")
		default:
			writeCheckoutErr(w, err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, sessionPublic(sess))
}

// sessionByAnon carrega a sessão exigindo o anon_token que identifica o navegador. O token
// deve corresponder EXATAMENTE ao gravado naquela sessão e a sessão deve estar 'open' (após
// o bind, o anon_token deixa de servir). Divergência ou ausência devolve 404 — nunca 403,
// que confirmaria que a sessão existe.
func (s *Server) sessionByAnon(w http.ResponseWriter, r *http.Request, tx pgx.Tx, id uuid.UUID) (checkout.Session, bool) {
	anon := r.Header.Get("X-Anon-Token")
	sess, err := checkout.GetSession(r.Context(), tx, id)
	if err != nil || anon == "" || sess.Status != "open" || !subtleCompare(sess.AnonToken, anon) {
		writeErr(w, http.StatusNotFound, "sessão não encontrada")
		return checkout.Session{}, false
	}
	return sess, true
}

// getSession retoma a sessão pela seleção intacta (sem auth, exige anon_token).
func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	producerID, err := s.producerOfSession(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "sessão não encontrada")
		return
	}
	var sess checkout.Session
	ok := true
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		var e error
		sess, ok = s.sessionByAnon(w, r, tx, id)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, sessionPublic(sess))
}

// patchSession altera a seleção (sem auth, exige anon_token) e re-reserva.
func (s *Server) patchSession(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var req checkout.Request
	if err := decode(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	attendees, problem := checkout.NormalizeAttendees(req.Attendees, req.Quantity, req.HalfPriceQty)
	if problem != "" {
		writeErr(w, http.StatusBadRequest, problem)
		return
	}
	req.Attendees = attendees
	producerID, err := s.producerOfSession(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "sessão não encontrada")
		return
	}
	var sess checkout.Session
	ok := true
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		if _, ok = s.sessionByAnon(w, r, tx, id); !ok {
			return nil
		}
		var e error
		sess, e = checkout.UpdateSession(r.Context(), tx, id, req, s.checkoutLimits)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, sessionPublic(sess))
}

// bindSession vincula a sessão ao comprador autenticado e estende a reserva uma vez.
func (s *Server) bindSession(w http.ResponseWriter, r *http.Request, subjectID uuid.UUID) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	producerID, err := s.producerOfSession(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "sessão não encontrada")
		return
	}
	err = s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		return checkout.BindSession(r.Context(), tx, id, subjectID, s.checkoutLimits)
	})
	switch {
	case errors.Is(err, checkout.ErrSessionBoundToOther):
		writeErr(w, http.StatusConflict, "sessão vinculada a outro comprador")
	case errors.Is(err, checkout.ErrSessionNotOpen), errors.Is(err, checkout.ErrSessionExpired):
		writeErr(w, http.StatusConflict, "sessão não está aberta")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// authStarted é chamado quando o código de acesso é pedido: estende a reserva e a sessão
// UMA vez por grace (cobre a janela lenta entre pedir o código e digitá-lo).
func (s *Server) authStarted(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	producerID, err := s.producerOfSession(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "sessão não encontrada")
		return
	}
	var ok bool
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		if _, ok = s.sessionByAnon(w, r, tx, id); !ok {
			return nil
		}
		return checkout.AuthStarted(r.Context(), tx, id, s.checkoutLimits)
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type paySessionReq struct {
	Method       string              `json:"method"`
	Installments int                 `json:"installments"`
	BuyerCPF     string              `json:"buyer_cpf"`
	Attendees    []checkout.Attendee `json:"attendees"`
}

// paySession cria a ordem/pagamento a partir da reserva da sessão. Só paga sessão vinculada
// ao subject do token. Dados do comprador vêm da conta; CPF é gravado no subject se pedido.
func (s *Server) paySession(w http.ResponseWriter, r *http.Request, subjectID uuid.UUID) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body paySessionReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	producerID, err := s.producerOfSession(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "sessão não encontrada")
		return
	}
	// Os dados da cobrança vêm da CONTA, não do corpo: quem paga é o cadastro autenticado,
	// e é dele que saem nome, documento e telefone do cliente no gateway.
	acc, err := s.buyerAccount(r.Context(), subjectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if acc.email == "" {
		writeErr(w, http.StatusBadRequest, "conta sem e-mail")
		return
	}
	if acc.cpf == "" && body.BuyerCPF == "" {
		writeErr(w, http.StatusBadRequest, "complete o cadastro (CPF) antes de pagar")
		return
	}
	email := acc.email
	var res checkout.Result
	err = s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		sess, e := checkout.GetSession(r.Context(), tx, id)
		if e != nil {
			return e
		}
		if sess.SubjectID == nil || *sess.SubjectID != subjectID {
			return checkout.ErrSessionNotOpen
		}
		if sess.Status != "authenticated" {
			return checkout.ErrSessionNotOpen
		}
		prod, e := store.GetProducer(r.Context(), tx, producerID)
		if e != nil {
			return e
		}
		cpf := acc.cpf
		if cpf == "" {
			cpf = body.BuyerCPF
		}
		// A ficha do pagamento vence a que a sessão guardava (é a tela imediatamente
		// anterior); sem ela, vale o que já foi preenchido na etapa dos participantes.
		attendees := sess.Items.Attendees
		if len(body.Attendees) > 0 {
			normalized, problem := checkout.NormalizeAttendees(body.Attendees, sess.Items.Quantity, sess.Items.HalfPriceQty)
			if problem != "" {
				return fmt.Errorf("%w: %s", checkout.ErrBadRequest, problem)
			}
			attendees = normalized
		}
		req := checkout.Request{
			Method: body.Method, Installments: body.Installments,
			SubjectID: subjectID, BuyerName: acc.name, BuyerEmail: email,
			BuyerCPF: cpf, BuyerPhone: acc.phone, Attendees: attendees,
		}
		if body.BuyerCPF != "" {
			if _, e := tx.Exec(r.Context(), `UPDATE public.subjects SET cpf=$2 WHERE id=$1`, subjectID, body.BuyerCPF); e != nil {
				return e
			}
		}
		res, e = checkout.PaySession(r.Context(), tx, s.seams.Payment, checkout.Producer{
			ID: prod.ID, RetentionPct: prod.RetentionPct, AsaasWalletID: prod.AsaasWalletID,
		}, sess, req)
		return e
	})
	switch {
	case errors.Is(err, checkout.ErrSessionNotFound):
		writeErr(w, http.StatusNotFound, "sessão não encontrada")
	case errors.Is(err, checkout.ErrSessionNotOpen):
		writeErr(w, http.StatusConflict, "sessão não vinculada a este comprador")
	case err != nil:
		writeCheckoutErr(w, err)
	default:
		writeJSON(w, http.StatusCreated, res)
	}
}

// producerOfSession resolve o produtor de uma sessão pelo índice público.
func (s *Server) producerOfSession(ctx context.Context, sessionID uuid.UUID) (uuid.UUID, error) {
	var pid uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT producer_id FROM public.checkout_session_index WHERE session_id=$1`, sessionID).Scan(&pid)
	return pid, err
}
