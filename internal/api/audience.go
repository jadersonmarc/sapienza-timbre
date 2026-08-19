package api

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/jadersonmarc/sapienza-timbre/internal/audience"
	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
)

type createSegmentReq struct {
	Name       string          `json:"name"`
	Definition json.RawMessage `json:"definition"`
}

func (s *Server) createSegment(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	var body createSegmentReq
	if err := decode(w, r, &body); err != nil || body.Name == "" {
		writeErr(w, http.StatusBadRequest, "name obrigatório")
		return
	}
	seg, err := audience.CreateSegment(r.Context(), s.pool, body.Name, body.Definition)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, claims, "segment.create", "segment", &seg.ID, map[string]any{"name": body.Name})
	writeJSON(w, http.StatusCreated, seg)
}

func (s *Server) recomputeSegment(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	segmentID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	size, err := audience.RecomputeSegment(r.Context(), s.pool, segmentID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"size": size})
}

type sponsorCampaignReq struct {
	Sponsor   string  `json:"sponsor"`
	SegmentID string  `json:"segment_id"`
	Budget    float64 `json:"budget"`
}

func (s *Server) createSponsorCampaign(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	var body sponsorCampaignReq
	if err := decode(w, r, &body); err != nil || body.Sponsor == "" {
		writeErr(w, http.StatusBadRequest, "sponsor obrigatório")
		return
	}
	segmentID, err := uuid.Parse(body.SegmentID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "segment_id inválido")
		return
	}
	c, err := audience.CreateSponsorCampaign(r.Context(), s.pool, body.Sponsor, segmentID, body.Budget)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, claims, "sponsor_campaign.create", "sponsor_campaign", &c.ID, map[string]any{"sponsor": body.Sponsor})
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) deliverCampaign(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	campaignID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	n, err := audience.Deliver(r.Context(), s.pool, campaignID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"delivered": n})
}

func (s *Server) campaignMetrics(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	campaignID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	m, err := audience.CampaignMetrics(r.Context(), s.pool, campaignID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, m)
}

type consentReq struct {
	SegmentID string `json:"segment_id"`
	Granted   bool   `json:"granted"`
}

// setConsent é o interruptor de consentimento do próprio público (público, granular,
// revogável). Recusar não restringe nada.
func (s *Server) setConsent(w http.ResponseWriter, r *http.Request) {
	subjectID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body consentReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	segmentID, err := uuid.Parse(body.SegmentID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "segment_id inválido")
		return
	}
	if err := audience.SetConsent(r.Context(), s.pool, subjectID, segmentID, body.Granted); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "granted": body.Granted})
}

// listConsents mostra os consentimentos vigentes do sujeito (o próprio dono).
func (s *Server) listConsents(w http.ResponseWriter, r *http.Request) {
	subjectID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT segment_id::text, granted, revoked_at IS NULL AS active
		  FROM consents WHERE subject_id=$1 AND granted AND revoked_at IS NULL`, subjectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	type c struct {
		SegmentID string `json:"segment_id"`
		Granted   bool   `json:"granted"`
		Active    bool   `json:"active"`
	}
	var out []c
	for rows.Next() {
		var it c
		if err := rows.Scan(&it.SegmentID, &it.Granted, &it.Active); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"consents": out})
}
