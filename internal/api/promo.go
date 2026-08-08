package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/promo"
)

type createCampaignReq struct {
	Name        string `json:"name"`
	UTMSource   string `json:"utm_source"`
	UTMMedium   string `json:"utm_medium"`
	UTMCampaign string `json:"utm_campaign"`
}

func (s *Server) createCampaign(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body createCampaignReq
	if err := decode(w, r, &body); err != nil || body.Name == "" {
		writeErr(w, http.StatusBadRequest, "name obrigatório")
		return
	}
	var out promo.Campaign
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = promo.CreateCampaign(r.Context(), tx, claims.ProducerID, eventID, body.Name, body.UTMSource, body.UTMMedium, body.UTMCampaign)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) listCampaigns(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var out []promo.Campaign
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = promo.ListCampaigns(r.Context(), tx, eventID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"campaigns": out})
}

func (s *Server) audienceProfile(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var out []promo.SourceStat
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = promo.AudienceProfile(r.Context(), tx, eventID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"by_source": out})
}

// campaignClick registra o clique de um link parametrizado (público). Resolve o produtor
// pelo índice público de campanhas.
func (s *Server) campaignClick(w http.ResponseWriter, r *http.Request) {
	campaignID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var producerID uuid.UUID
	err = s.pool.QueryRow(r.Context(), `SELECT producer_id FROM public.campaign_index WHERE campaign_id=$1`, campaignID).Scan(&producerID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "campanha não encontrada")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		return promo.TrackClick(r.Context(), tx, campaignID)
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type joinWaitlistReq struct {
	Email string `json:"email"`
}

// joinWaitlist inscreve na lista de espera (público). Resolve o produtor pelo diretório.
func (s *Server) joinWaitlist(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body joinWaitlistReq
	if err := decode(w, r, &body); err != nil || body.Email == "" {
		writeErr(w, http.StatusBadRequest, "email obrigatório")
		return
	}
	producerID, err := s.producerOfEvent(r.Context(), eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "evento não encontrado")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		return promo.JoinWaitlist(r.Context(), tx, eventID, body.Email)
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

// eventPixels devolve os pixels do evento (público, para a página de checkout).
func (s *Server) eventPixels(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	producerID, err := s.producerOfEvent(r.Context(), eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "evento não encontrado")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var px promo.Pixels
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		var e error
		px, e = promo.EventPixels(r.Context(), tx, eventID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, px)
}
