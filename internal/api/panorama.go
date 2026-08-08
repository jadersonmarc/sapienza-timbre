package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/jadersonmarc/sapienza-timbre/internal/panorama"
)

// subjectPanorama é a peça compartilhável do público: mapa/linha do tempo dos lugares
// por onde passou + retrospectiva do ano. Pública (o dono compartilha o link).
func (s *Server) subjectPanorama(w http.ResponseWriter, r *http.Request) {
	subjectID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	year := time.Now().Year()
	if q := r.URL.Query().Get("year"); q != "" {
		if y, err := strconv.Atoi(q); err == nil {
			year = y
		}
	}
	places, err := panorama.Places(r.Context(), s.pool, subjectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	retro, err := panorama.Retrospective(r.Context(), s.pool, subjectID, year)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"places": places, "retrospective": retro})
}
