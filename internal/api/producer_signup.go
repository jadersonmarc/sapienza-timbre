package api

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
)

type producerSignupReq struct {
	Name          string `json:"name"`
	OwnerEmail    string `json:"owner_email"`
	OwnerPassword string `json:"owner_password"`
	// AsaasWalletID é a carteira de recebimento: é para onde o gateway manda a parte do
	// produtor em cada venda. Opcional no cadastro (nem todo mundo já tem a conta pronta),
	// mas sem ela o evento não pode ser publicado — o dinheiro não teria para onde ir.
	AsaasWalletID string `json:"asaas_wallet_id"`
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
	wallet := strings.TrimSpace(body.AsaasWalletID)
	if wallet != "" && !validWalletID(wallet) {
		writeErr(w, http.StatusBadRequest, "carteira de recebimento inválida (use o Wallet ID da sua conta Asaas)")
		return
	}
	res, err := s.prov.CreateWithWallet(r.Context(), name, email, body.OwnerPassword, wallet)
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
	message := "Produtor criado. Sua conta já está ativa — acesse o painel para publicar eventos."
	if wallet == "" {
		message = "Produtor criado. Antes de publicar seu primeiro evento, informe no painel a carteira de recebimento — é para onde o dinheiro das vendas vai."
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"producer": res.Producer, "owner": res.Owner, "token": tok,
		"needs_wallet": wallet == "",
		"message":      message,
	})
}

// validWalletID confere o formato do Wallet ID do Asaas (UUID). Guarda de digitação: um
// identificador torto só apareceria como split silenciosamente perdido lá na frente.
// PROVISÓRIO: se o gateway passar a emitir outro formato, é aqui que se afrouxa.
func validWalletID(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}
