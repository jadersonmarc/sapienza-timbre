package api

import (
	"net/http"
	"strings"
)

// artistSignup é o cadastro público de artista (catálogo global). Self-service: o
// artista nasce ATIVO e entra no catálogo na hora, sem aprovação manual — moderação
// reativa (suspensão) se houver denúncia.
func (s *Server) artistSignup(w http.ResponseWriter, r *http.Request) {
	var body artistReq
	if err := decode(w, r, &body); err != nil || strings.TrimSpace(body.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name obrigatório")
		return
	}
	a, err := s.insertArtist(r.Context(), strings.TrimSpace(body.Name), body.Bio, body.ImageURL, body.Category)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, a)
}
