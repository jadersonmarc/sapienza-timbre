package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/checkout"
	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
	"github.com/jadersonmarc/sapienza-timbre/internal/store"
	"github.com/jadersonmarc/sapienza-timbre/internal/subaccount"
)

// Faturamento mensal declarado: o gateway usa para o perfil de movimentação da conta. O
// piso é só uma guarda de digitação — PROVISÓRIO, isolado aqui.
const minIncomeCents int64 = 100_00

type receivingAccountReq struct {
	// Titular da conta que vai receber o dinheiro das vendas.
	LegalName   string `json:"legal_name"`
	TaxID       string `json:"tax_id"` // CPF ou CNPJ
	Email       string `json:"email"`
	MobilePhone string `json:"mobile_phone"`
	BirthDate   string `json:"birth_date"`   // pessoa física
	CompanyType string `json:"company_type"` // pessoa jurídica: MEI|LIMITED|INDIVIDUAL|ASSOCIATION
	IncomeCents int64  `json:"income_cents"`
	PostalCode  string `json:"postal_code"`
	Address     string `json:"address"`
	Number      string `json:"address_number"`
	Province    string `json:"province"`
	// WalletID permite pular a abertura de conta quando o produtor JÁ tem conta no
	// gateway: informa o identificador dela e pronto.
	WalletID string `json:"wallet_id"`
}

// receivingAccount registra para onde vai o dinheiro do produtor. Dois caminhos: quem já
// tem conta no gateway informa o identificador dela; quem não tem, manda os dados e a
// conta é aberta aqui — sem sair do painel e sem abrir conta em outro lugar.
//
// É pré-requisito de publicar (o guarda em publishEvent), e não de cadastrar: pedir tudo
// isso na primeira tela mataria a captação de quem só quer ver o produto por dentro.
func (s *Server) receivingAccount(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	var body receivingAccountReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	ctx := r.Context()

	// Caminho curto: já tem conta no gateway.
	if wallet := strings.TrimSpace(body.WalletID); wallet != "" {
		if !validWalletID(wallet) {
			writeErr(w, http.StatusBadRequest, "identificador de carteira inválido")
			return
		}
		if err := store.SetAsaasWallet(ctx, s.pool, claims.ProducerID, wallet); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "wallet_id": wallet, "created": false})
		return
	}

	in, problem := parseReceivingAccount(body)
	if problem != "" {
		writeErr(w, http.StatusBadRequest, problem)
		return
	}
	acc, err := s.seams.Subaccounts.Create(ctx, claims.ProducerID, in)
	if errors.Is(err, subaccount.ErrLimitReached) {
		// Teto do período de avaliação regulatória: não é erro do produtor e não adianta
		// ele tentar de novo — é a plataforma que precisa resolver com o gateway.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "no momento não estamos abrindo novas contas de recebimento. Fale com o suporte.",
		})
		return
	}
	if err != nil {
		// A recusa vem do gateway (documento já usado, dado inconsistente) e é do produtor
		// resolver — devolver 500 esconderia isso atrás de "erro interno".
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "não foi possível abrir a conta de recebimento: " + err.Error(),
		})
		return
	}
	// A validação do documento junto à Receita ainda está correndo: consultar os documentos
	// agora devolveria pendências erradas. A espera acontece fora da requisição para não
	// segurar o produtor na tela.
	go s.syncDocumentsAfterDelay(claims.ProducerID)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "wallet_id": acc.WalletID, "created": true,
		"account_status": acc.Status,
	})
}

// receivingAccountStatus diz se o produtor já pode vender. O painel usa para avisar antes
// de o produtor descobrir na hora de publicar.
func (s *Server) receivingAccountStatus(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	prod, err := store.GetProducer(r.Context(), s.pool, claims.ProducerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	acc, err := s.seams.Subaccounts.Get(r.Context(), claims.ProducerID)
	if err != nil && !errors.Is(err, subaccount.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = prod
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":                 acc.CanSell(),
		"account_status":             statusOrNone(acc.Status),
		"onboarding_url":             acc.OnboardingURL,
		"can_sell":                   acc.CanSell(),
		"commercial_info_expires_at": acc.CommercialInfoExpiresAt,
	})
}

// parseReceivingAccount valida o que o gateway exige, em português e um problema por vez.
func parseReceivingAccount(b receivingAccountReq) (payment.AccountInput, string) {
	var in payment.AccountInput
	in.Name = strings.Join(strings.Fields(b.LegalName), " ")
	if in.Name == "" {
		return in, "informe o nome ou a razão social do titular"
	}
	in.TaxID = onlyDigits(b.TaxID)
	switch len(in.TaxID) {
	case 11:
		if !checkout.ValidCPF(in.TaxID) {
			return in, "CPF inválido"
		}
		if _, err := time.Parse("2006-01-02", strings.TrimSpace(b.BirthDate)); err != nil {
			return in, "informe a data de nascimento do titular"
		}
		in.BirthDate = strings.TrimSpace(b.BirthDate)
	case 14:
		in.CompanyType = strings.ToUpper(strings.TrimSpace(b.CompanyType))
		if !validCompanyType(in.CompanyType) {
			return in, "informe o tipo de empresa (MEI, LIMITED, INDIVIDUAL ou ASSOCIATION)"
		}
	default:
		return in, "informe um CPF ou CNPJ válido"
	}
	in.Email = normalizeEmail(b.Email)
	if !looksLikeEmail(in.Email) {
		return in, "e-mail inválido"
	}
	in.MobilePhone = onlyDigits(b.MobilePhone)
	if len(in.MobilePhone) < 10 || len(in.MobilePhone) > 11 {
		return in, "celular inválido (informe DDD e número)"
	}
	if b.IncomeCents < minIncomeCents {
		return in, "informe o faturamento mensal aproximado"
	}
	in.IncomeCents = b.IncomeCents
	in.PostalCode = onlyDigits(b.PostalCode)
	if len(in.PostalCode) != 8 {
		return in, "CEP inválido"
	}
	in.Address = strings.TrimSpace(b.Address)
	in.AddressNumber = strings.TrimSpace(b.Number)
	in.Province = strings.TrimSpace(b.Province)
	if in.Address == "" || in.AddressNumber == "" || in.Province == "" {
		return in, "informe endereço, número e bairro"
	}
	return in, ""
}

// validCompanyType lista os tipos aceitos pelo gateway.
func validCompanyType(t string) bool {
	switch t {
	case "MEI", "LIMITED", "INDIVIDUAL", "ASSOCIATION":
		return true
	}
	return false
}

// syncDocumentsAfterDelay espera a validação do documento terminar no gateway e então busca
// as pendências e o link de onboarding. Roda destacado da requisição: quem cadastrou não
// precisa esperar por isso, e o link aparece no painel quando estiver pronto.
func (s *Server) syncDocumentsAfterDelay(producerID uuid.UUID) {
	svc := s.seams.Subaccounts
	if svc == nil {
		return
	}
	time.Sleep(svc.DocumentsDelay)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := svc.SyncDocuments(ctx, producerID); err != nil {
		slog.Warn("subconta: não foi possível buscar as pendências de documentação",
			"producer_id", producerID, "err", err)
	}
}
