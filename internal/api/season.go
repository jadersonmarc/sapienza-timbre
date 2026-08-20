package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/season"
)

type createPassReq struct {
	Name       string `json:"name"`
	PriceCents int64  `json:"price_cents"`
}

func (s *Server) createPass(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	var body createPassReq
	if err := decode(w, r, &body); err != nil || body.Name == "" || body.PriceCents <= 0 {
		writeErr(w, http.StatusBadRequest, "name e price_cents (> 0) obrigatórios")
		return
	}
	var out season.Pass
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = season.CreatePass(r.Context(), tx, body.Name, body.PriceCents)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

type addDateReq struct {
	EventID      string  `json:"event_id"`
	LotID        string  `json:"lot_id"`
	OccursAt     *string `json:"occurs_at"`
	Detachable   *bool   `json:"detachable"`
	Transferable *bool   `json:"transferable"`
}

func (s *Server) addPassDate(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	passID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body addDateReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	eventID, err := uuid.Parse(body.EventID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "event_id inválido")
		return
	}
	lotID, err := uuid.Parse(body.LotID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "lot_id inválido")
		return
	}
	occurs, err := parseTimePtr(body.OccursAt)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "occurs_at inválido (use RFC3339)")
		return
	}
	detachable, transferable := true, true
	if body.Detachable != nil {
		detachable = *body.Detachable
	}
	if body.Transferable != nil {
		transferable = *body.Transferable
	}
	var out season.Date
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = season.AddDate(r.Context(), tx, passID, eventID, lotID, occurs, detachable, transferable)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

type buyPassReq struct {
	BuyerEmail string `json:"buyer_email"`
}

// buyPass é a compra de um passe pelo comprador AUTENTICADO (cadastro obrigatório).
// Resolve o produtor pelo diretório público (o passe pertence a um evento de referência).
func (s *Server) buyPass(w http.ResponseWriter, r *http.Request, subjectID uuid.UUID) {
	passID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	producerID, err := uuid.Parse(r.URL.Query().Get("producer"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "parâmetro producer obrigatório")
		return
	}
	var body buyPassReq
	_ = decode(w, r, &body)
	email, err := s.buyerEmail(r.Context(), subjectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var res season.BuyResult
	err = s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		var e error
		res, e = season.BuyPass(r.Context(), tx, s.seams.Payment, producerID, passID, email)
		return e
	})
	if errors.Is(err, season.ErrNoDates) {
		writeErr(w, http.StatusConflict, "passe sem datas")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, res)
}
