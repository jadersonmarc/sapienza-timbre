package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/jadersonmarc/sapienza-timbre/internal/trust"
)

type reviewReq struct {
	Rating int    `json:"rating"`
	Body   string `json:"body"`
}

// submitReview registra uma avaliação — restrita a quem fez check-in (o check-in é a
// prova). Público (o avaliador é o próprio público, sem conta).
func (s *Server) submitReview(w http.ResponseWriter, r *http.Request) {
	checkinID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body reviewReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	id, err := trust.SubmitReview(r.Context(), s.pool, checkinID, body.Rating, body.Body)
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, map[string]any{"id": id})
	case errors.Is(err, trust.ErrNotAttended):
		writeErr(w, http.StatusForbidden, "avaliação restrita a quem fez check-in")
	case errors.Is(err, trust.ErrAlreadyReviewed):
		writeErr(w, http.StatusConflict, "check-in já avaliado")
	case errors.Is(err, trust.ErrBadRating):
		writeErr(w, http.StatusBadRequest, "nota deve ser de 1 a 5")
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) producerReputation(w http.ResponseWriter, r *http.Request) {
	producerID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	rep, err := trust.RecomputeReputation(r.Context(), s.pool, producerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	reviews, avg, err := trust.ProducerReviews(r.Context(), s.pool, producerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reputation": rep, "rating_avg": avg, "reviews": reviews})
}

func (s *Server) subjectDiscovery(w http.ResponseWriter, r *http.Request) {
	subjectID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	sug, err := trust.Discovery(r.Context(), s.pool, subjectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"suggestions": sug})
}
