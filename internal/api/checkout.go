package api

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/checkout"
	"github.com/jadersonmarc/sapienza-timbre/internal/inventory"
	"github.com/jadersonmarc/sapienza-timbre/internal/market"
	"github.com/jadersonmarc/sapienza-timbre/internal/store"
)

// publicCheckout é a compra PÚBLICA (o comprador sequer tem conta). Resolve o produtor
// pelo diretório público (event_directory) e roda o checkout no schema dele.
func (s *Server) publicCheckout(w http.ResponseWriter, r *http.Request) {
	var req checkout.Request
	if err := decode(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if req.EventID == uuid.Nil {
		writeErr(w, http.StatusBadRequest, "event_id obrigatório")
		return
	}
	producerID, err := s.producerOfEvent(r.Context(), req.EventID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "evento não encontrado ou não publicado")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	var res checkout.Result
	err = s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		prod, e := store.GetProducer(r.Context(), tx, producerID)
		if e != nil {
			return e
		}
		res, e = checkout.StartCheckout(r.Context(), tx, s.seams.Payment, checkout.Producer{
			ID: prod.ID, RetentionPct: prod.RetentionPct, AsaasWalletID: prod.AsaasWalletID,
		}, req)
		return e
	})
	if err != nil {
		writeCheckoutErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// asaasWebhook recebe a confirmação de pagamento (endpoint global). Idempotente:
// resolve o produtor pelo payment_index e confirma. Valida o header quando há token.
func (s *Server) asaasWebhook(w http.ResponseWriter, r *http.Request) {
	if s.webhookToken != "" && !subtleCompare(r.Header.Get("asaas-access-token"), s.webhookToken) {
		writeErr(w, http.StatusUnauthorized, "token de webhook inválido")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	evt, err := s.seams.Payment.HandleWebhook(r.Context(), body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "webhook ilegível")
		return
	}
	// Interessam confirmação e estorno; os demais são reconhecidos com 200.
	if evt.AsaasRef == "" || (!evt.Confirmed && !evt.Refunded) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	producerID, kind, err := s.producerOfPayment(r.Context(), evt.AsaasRef)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true}) // ref desconhecida: ack
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	em := s.emitter()
	chainOn := s.seams.Chain.Enabled()
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		switch {
		case evt.Refunded:
			return checkout.RefundPayment(r.Context(), tx, evt.AsaasRef)
		case kind == "resale":
			return market.ConfirmResale(r.Context(), tx, producerID, evt.AsaasRef, chainOn)
		default:
			_, e := checkout.ConfirmPayment(r.Context(), tx, em, evt.AsaasRef)
			return e
		}
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) producerOfEvent(ctx context.Context, eventID uuid.UUID) (uuid.UUID, error) {
	var pid uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT producer_id FROM public.event_directory WHERE event_id = $1`, eventID).Scan(&pid)
	return pid, err
}

func (s *Server) producerOfPayment(ctx context.Context, asaasRef string) (uuid.UUID, string, error) {
	var pid uuid.UUID
	var kind string
	err := s.pool.QueryRow(ctx, `SELECT producer_id, kind FROM public.payment_index WHERE asaas_ref = $1`, asaasRef).Scan(&pid, &kind)
	return pid, kind, err
}

// writeCheckoutErr mapeia os erros de domínio do checkout para status HTTP.
func writeCheckoutErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, checkout.ErrLotUnavailable):
		writeErr(w, http.StatusConflict, "lote indisponível")
	case errors.Is(err, checkout.ErrInsufficientStock), errors.Is(err, inventory.ErrSeatUnavailable):
		writeErr(w, http.StatusConflict, "ingressos/assentos indisponíveis")
	case errors.Is(err, checkout.ErrSeatsMismatch), errors.Is(err, checkout.ErrCoupon),
		errors.Is(err, checkout.ErrBadRequest), errors.Is(err, inventory.ErrSeatInvalid),
		errors.Is(err, inventory.ErrSeatBlocked), errors.Is(err, inventory.ErrAntiHole):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

// ── lista de convidados / cortesias ──────────────────────────────────────────

type createGuestReq struct {
	Name   string  `json:"name"`
	CPF    string  `json:"cpf"`
	LotID  *string `json:"lot_id"`
	SeatID *string `json:"seat_id"`
}

// createGuest emite uma cortesia: registra o convidado e emite um ingresso (com
// assento específico opcional, ocupando-o para não ser vendido).
func (s *Server) createGuest(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body createGuestReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if body.Name == "" {
		writeErr(w, http.StatusBadRequest, "name obrigatório")
		return
	}
	lotID, err := parseUUIDPtr(body.LotID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "lot_id inválido")
		return
	}
	seatID, err := parseUUIDPtr(body.SeatID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "seat_id inválido")
		return
	}
	var ticketID uuid.UUID
	em := s.emitter()
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		ticketID, e = checkout.IssueCourtesy(r.Context(), tx, em, eventID, lotID, seatID, body.Name, body.CPF)
		return e
	}); err != nil {
		if errors.Is(err, inventory.ErrSeatUnavailable) {
			writeErr(w, http.StatusConflict, "assento indisponível")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ticket_id": ticketID})
}

func (s *Server) listGuests(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	type guest struct {
		ID     uuid.UUID `json:"id"`
		Name   string    `json:"name"`
		CPF    *string   `json:"cpf,omitempty"`
		SeatID *string   `json:"seat_id,omitempty"`
		Status string    `json:"status"`
	}
	var out []guest
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		rows, e := tx.Query(r.Context(), `
			SELECT id, name, cpf, seat_id::text, status
			  FROM guest_list_entries WHERE event_id=$1 ORDER BY created_at`, eventID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var g guest
			if e := rows.Scan(&g.ID, &g.Name, &g.CPF, &g.SeatID, &g.Status); e != nil {
				return e
			}
			out = append(out, g)
		}
		return rows.Err()
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"guests": out})
}

func parseUUIDPtr(s *string) (*uuid.UUID, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	id, err := uuid.Parse(*s)
	if err != nil {
		return nil, err
	}
	return &id, nil
}
