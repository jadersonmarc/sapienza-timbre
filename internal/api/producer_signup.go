package api

import (
	"net/http"
	"strings"
)

type producerSignupReq struct {
	Name          string `json:"name"`
	OwnerEmail    string `json:"owner_email"`
	OwnerPassword string `json:"owner_password"`
}

// producerSignup é o destino da landing B2B (§3.12): cadastro PÚBLICO (sem X-Admin-Token)
// que cria o produtor PENDENTE de aprovação. O owner já pode logar; o produtor entra na
// fila de aprovação do admin (POST /admin/producers/{id}/approve).
func (s *Server) producerSignup(w http.ResponseWriter, r *http.Request) {
	var body producerSignupReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	name := strings.TrimSpace(body.Name)
	email := normalizeEmail(body.OwnerEmail)
	if name == "" || !looksLikeEmail(email) || len(body.OwnerPassword) < 8 {
		writeErr(w, http.StatusBadRequest, "name, owner_email e owner_password (mín. 8) obrigatórios")
		return
	}
	res, err := s.prov.CreatePending(r.Context(), name, email, body.OwnerPassword)
	if err != nil {
		if isUniqueViolation(err) {
			writeErr(w, http.StatusConflict, "e-mail já cadastrado")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"producer": res.Producer, "owner": res.Owner,
		"message": "Cadastro recebido. Seu produtor está em análise e será liberado após aprovação.",
	})
}
