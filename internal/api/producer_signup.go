package api

import (
	"net/http"
	"strings"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
)

type producerSignupReq struct {
	Name          string `json:"name"`
	OwnerEmail    string `json:"owner_email"`
	OwnerPassword string `json:"owner_password"`
}

// producerSignup é o destino da landing B2B (§3.12): cadastro PÚBLICO (sem X-Admin-Token)
// que cria o produtor ATIVO na hora (self-service) e já devolve o token do owner. A
// moderação é REATIVA: produtor só é suspenso se houver denúncia/comportamento indevido.
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
	res, err := s.prov.Create(r.Context(), name, email, body.OwnerPassword)
	if err != nil {
		if isUniqueViolation(err) {
			writeErr(w, http.StatusConflict, "e-mail já cadastrado")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	tok, err := s.auth.Issue(auth.Identity{
		CollaboratorID: res.Owner.ID,
		ProducerID:     res.Producer.ID,
		Owner:          true,
		SessionVersion: res.Owner.SessionVersion,
		Permissions:    []string{},
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// O produtor não abre conta em gateway nenhum: a bilheteria recebe e repassa depois do
	// evento. O que falta cadastrar é só para onde transferir.
	writeJSON(w, http.StatusCreated, map[string]any{
		"producer": res.Producer, "owner": res.Owner, "token": tok,
		"needs_wallet": true,
		"message": "Produtor criado. Antes de publicar seu primeiro evento, informe no painel a chave Pix de recebimento — " +
			"é para lá que o dinheiro das vendas é transferido depois que o evento acontece.",
	})
}
