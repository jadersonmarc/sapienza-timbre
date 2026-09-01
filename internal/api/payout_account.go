package api

import (
	"net/http"
	"strings"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/checkout"
	"github.com/jadersonmarc/sapienza-timbre/internal/store"
)

type payoutAccountReq struct {
	PixKey      string `json:"pix_key"`
	PixKeyType  string `json:"pix_key_type"` // cpf|cnpj|email|phone|random
	HolderName  string `json:"holder_name"`
	HolderTaxID string `json:"holder_tax_id"`
}

// payoutAccount grava para onde a plataforma transfere a parte do produtor. A bilheteria
// recebe o total e repassa depois da realização do evento, e para isso basta uma chave Pix:
// o produtor não abre conta no gateway.
//
// O titular da chave é conferido contra o documento informado: transferir para chave de
// terceiro embaralha a responsabilidade fiscal de quem recebeu.
func (s *Server) payoutAccount(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	var body payoutAccountReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	acc, problem := parsePayoutAccount(body)
	if problem != "" {
		writeErr(w, http.StatusBadRequest, problem)
		return
	}
	if err := store.SetPayoutAccount(r.Context(), s.pool, claims.ProducerID, acc); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// payoutAccountStatus diz se o produtor já pode receber (e como). O painel usa para avisar
// antes de o produtor esbarrar no guarda de publicação.
func (s *Server) payoutAccountStatus(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	acc, err := store.GetPayoutAccount(r.Context(), s.pool, claims.ProducerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": acc.PixKey != "",
		// Só existe um modo: a bilheteria retém e transfere depois do evento. O campo
		// continua no corpo porque o painel o lê; o que sumiu foi o outro valor possível.
		"mode":         payoutMode(acc.PixKey != ""),
		"pix_key":      maskPixKey(acc.PixKey),
		"pix_key_type": acc.PixKeyType,
		"holder_name":  acc.HolderName,
	})
}

func payoutMode(pix bool) string {
	if pix {
		return "payout"
	}
	return "none"
}

// maskPixKey esconde o miolo da chave: confirma para o produtor que é a dele sem exibir o
// dado inteiro numa tela que pode estar sendo compartilhada.
func maskPixKey(key string) string {
	if len(key) <= 6 {
		return key
	}
	return key[:3] + strings.Repeat("•", len(key)-6) + key[len(key)-3:]
}

// parsePayoutAccount valida a chave conforme o tipo. Chave torta só apareceria como
// transferência devolvida, dias depois, com o produtor cobrando.
func parsePayoutAccount(b payoutAccountReq) (store.PayoutAccount, string) {
	var acc store.PayoutAccount
	acc.HolderName = strings.Join(strings.Fields(b.HolderName), " ")
	if len(strings.Fields(acc.HolderName)) < 2 {
		return acc, "informe o nome completo do titular da chave"
	}
	acc.HolderTaxID = onlyDigits(b.HolderTaxID)
	switch len(acc.HolderTaxID) {
	case 11:
		if !checkout.ValidCPF(acc.HolderTaxID) {
			return acc, "CPF do titular inválido"
		}
	case 14: // CNPJ: guarda de tamanho; a conferência real é do banco no destino
	default:
		return acc, "informe o CPF ou CNPJ do titular"
	}

	acc.PixKeyType = strings.ToLower(strings.TrimSpace(b.PixKeyType))
	key := strings.TrimSpace(b.PixKey)
	switch acc.PixKeyType {
	case "cpf":
		key = onlyDigits(key)
		if !checkout.ValidCPF(key) {
			return acc, "chave CPF inválida"
		}
		if key != acc.HolderTaxID {
			return acc, "a chave CPF precisa ser a do titular informado"
		}
	case "cnpj":
		key = onlyDigits(key)
		if len(key) != 14 {
			return acc, "chave CNPJ inválida"
		}
		if key != acc.HolderTaxID {
			return acc, "a chave CNPJ precisa ser a do titular informado"
		}
	case "email":
		key = strings.ToLower(key)
		if !looksLikeEmail(key) {
			return acc, "chave de e-mail inválida"
		}
	case "phone":
		key = onlyDigits(key)
		if len(key) < 10 || len(key) > 11 {
			return acc, "chave de celular inválida (informe DDD e número)"
		}
	case "random":
		if len(key) != 36 {
			return acc, "chave aleatória inválida"
		}
	default:
		return acc, "escolha o tipo da chave Pix"
	}
	acc.PixKey = key
	return acc, ""
}
