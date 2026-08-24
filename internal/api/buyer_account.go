package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/notify"
)

// Regras do cadastro do comprador. PROVISÓRIAS e isoladas: o mínimo de senha segue a
// recomendação corrente (comprimento acima de complexidade), a idade mínima evita conta de
// criança sem responsável e o telefone aceita fixo ou celular brasileiro.
const (
	minPasswordLen = 8
	minAgeYears    = 16
)

var (
	digitsOnly = regexp.MustCompile(`\D`)
	nameParts  = regexp.MustCompile(`\S+`)
)

type registerReq struct {
	Name      string `json:"name"`
	Email     string `json:"email"`
	CPF       string `json:"cpf"`
	Phone     string `json:"phone"`
	BirthDate string `json:"birth_date"` // AAAA-MM-DD
	Password  string `json:"password"`
}

// register cria a conta do comprador com os dados que a compra exige. Diferente do código
// por e-mail, aqui a pessoa diz quem é — e é esse cadastro que alimenta o cliente da
// cobrança, a meia-entrada e o contato no dia do evento.
//
// A conta nasce utilizável: o e-mail é verificado por código DEPOIS, em paralelo, para a
// verificação não travar uma compra em andamento.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var body registerReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	acc, problem := parseAccount(body)
	if problem != "" {
		writeErr(w, http.StatusBadRequest, problem)
		return
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "erro ao criar conta")
		return
	}
	ctx := r.Context()

	// Uma conta por e-mail. Se já existe SEM senha (criada por código ou por compra
	// anterior), o cadastro a completa em vez de recusar — recusar deixaria a pessoa
	// presa, com uma conta que ela não sabe que tem.
	var subjectID uuid.UUID
	err = s.pool.QueryRow(ctx, `
		INSERT INTO subjects (display_name, email, cpf, phone, birth_date, password_hash)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id`,
		acc.name, acc.email, acc.cpf, acc.phone, acc.birthDate, hash).Scan(&subjectID)
	if isUniqueViolation(err) {
		var existingHash *string
		if e := s.pool.QueryRow(ctx, `SELECT id, password_hash FROM subjects WHERE lower(email)=$1`, acc.email).
			Scan(&subjectID, &existingHash); e != nil {
			writeErr(w, http.StatusInternalServerError, "erro ao criar conta")
			return
		}
		if existingHash != nil && *existingHash != "" {
			writeErr(w, http.StatusConflict, "já existe uma conta com este e-mail")
			return
		}
		if _, e := s.pool.Exec(ctx, `
			UPDATE subjects SET display_name=$2, cpf=$3, phone=$4, birth_date=$5, password_hash=$6, updated_at=now()
			 WHERE id=$1`, subjectID, acc.name, acc.cpf, acc.phone, acc.birthDate, hash); e != nil {
			writeErr(w, http.StatusInternalServerError, "erro ao criar conta")
			return
		}
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "erro ao criar conta")
		return
	}

	// Ingressos comprados antes do cadastro, com o mesmo e-mail, só são vinculados após a
	// verificação (§3.4) — o cadastro sozinho não prova posse do endereço.
	s.sendVerificationCode(ctx, acc.email)

	tok, err := s.auth.IssueBuyer(subjectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "erro ao emitir sessão")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "subject_id": subjectID})
}

type buyerLoginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// login autentica pela senha. A resposta não distingue e-mail inexistente de senha errada
// (§4.1) — as duas devolvem a mesma coisa, senão a rota vira um verificador de cadastro.
func (s *Server) buyerLogin(w http.ResponseWriter, r *http.Request) {
	var body buyerLoginReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	email := normalizeEmail(body.Email)
	var subjectID uuid.UUID
	var hash *string
	err := s.pool.QueryRow(r.Context(), `SELECT id, password_hash FROM subjects WHERE lower(email)=$1`, email).
		Scan(&subjectID, &hash)
	if err != nil || hash == nil || !auth.ComparePassword(*hash, body.Password) {
		writeErr(w, http.StatusUnauthorized, "e-mail ou senha inválidos")
		return
	}
	tok, err := s.auth.IssueBuyer(subjectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "erro ao emitir sessão")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "subject_id": subjectID})
}

type updateMeReq struct {
	Name      string `json:"name"`
	CPF       string `json:"cpf"`
	Phone     string `json:"phone"`
	BirthDate string `json:"birth_date"`
}

// updateMe corrige os dados da conta. E-mail não muda por aqui: trocar o endereço muda a
// identidade da conta e exigiria nova verificação.
func (s *Server) updateMe(w http.ResponseWriter, r *http.Request, subjectID uuid.UUID) {
	var body updateMeReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	acc, problem := parseAccount(registerReq{
		Name: body.Name, CPF: body.CPF, Phone: body.Phone, BirthDate: body.BirthDate,
		Email: "ignorado@exemplo.com", Password: strings.Repeat("x", minPasswordLen),
	})
	if problem != "" {
		writeErr(w, http.StatusBadRequest, problem)
		return
	}
	if _, err := s.pool.Exec(r.Context(), `
		UPDATE subjects SET display_name=$2, cpf=$3, phone=$4, birth_date=$5, updated_at=now()
		 WHERE id=$1`, subjectID, acc.name, acc.cpf, acc.phone, acc.birthDate); err != nil {
		writeErr(w, http.StatusInternalServerError, "erro ao salvar")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// account são os dados já normalizados do cadastro.
type account struct {
	name      string
	email     string
	cpf       string
	phone     string
	birthDate time.Time
}

// parseAccount valida e normaliza o cadastro, devolvendo a primeira pendência em texto que
// o comprador entenda. Guarda de verdade, não de fachada: CPF inválido e telefone curto são
// recusados pelo gateway lá na frente, quando o dinheiro já está em jogo.
func parseAccount(b registerReq) (account, string) {
	var a account
	a.name = strings.Join(nameParts.FindAllString(strings.TrimSpace(b.Name), -1), " ")
	if len(nameParts.FindAllString(a.name, -1)) < 2 {
		return a, "informe o nome completo"
	}
	a.email = normalizeEmail(b.Email)
	if !looksLikeEmail(a.email) {
		return a, "e-mail inválido"
	}
	a.cpf = digitsOnly.ReplaceAllString(b.CPF, "")
	if !validCPF(a.cpf) {
		return a, "CPF inválido"
	}
	a.phone = digitsOnly.ReplaceAllString(b.Phone, "")
	if len(a.phone) < 10 || len(a.phone) > 11 {
		return a, "telefone inválido (informe DDD e número)"
	}
	birth, err := time.Parse("2006-01-02", strings.TrimSpace(b.BirthDate))
	if err != nil {
		return a, "data de nascimento inválida"
	}
	if birth.After(time.Now().AddDate(-minAgeYears, 0, 0)) {
		return a, "é preciso ter ao menos 16 anos para comprar"
	}
	a.birthDate = birth
	if len(b.Password) < minPasswordLen {
		return a, "a senha precisa de ao menos 8 caracteres"
	}
	return a, ""
}

// validCPF confere os dois dígitos verificadores. Rejeita também os repetidos (111...),
// que passam no cálculo mas não existem.
func validCPF(cpf string) bool {
	if len(cpf) != 11 {
		return false
	}
	allSame := true
	for i := 1; i < 11; i++ {
		if cpf[i] != cpf[0] {
			allSame = false
			break
		}
	}
	if allSame {
		return false
	}
	for _, pos := range []int{9, 10} {
		sum := 0
		for i := 0; i < pos; i++ {
			sum += int(cpf[i]-'0') * (pos + 1 - i)
		}
		d := (sum * 10) % 11
		if d == 10 {
			d = 0
		}
		if d != int(cpf[pos]-'0') {
			return false
		}
	}
	return true
}

// sendVerificationCode enfileira o código de verificação do e-mail. Best-effort: falhar
// aqui não pode impedir o cadastro (a pessoa reenvia depois).
func (s *Server) sendVerificationCode(ctx context.Context, email string) {
	code := numericCode(otpLength)
	hash, err := auth.HashPassword(code)
	if err != nil {
		return
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO buyer_otps (email, code_hash, expires_at)
		VALUES ($1,$2, now() + make_interval(secs => $3))`, email, hash, otpTTL.Seconds()); err != nil {
		return
	}
	_ = s.seams.Notify.Send(ctx, notify.Message{
		Kind: notify.KindAuthCode, Channel: "email", To: email,
		Code: code, CodeMinutes: int(otpTTL.Minutes()),
	})
}

// buyerAccount carrega os dados da conta para preencher o cliente da cobrança.
func (s *Server) buyerAccount(ctx context.Context, subjectID uuid.UUID) (account, error) {
	var a account
	var name, email, cpf, phone *string
	var birth *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT display_name, email, cpf, phone, birth_date FROM subjects WHERE id=$1`, subjectID).
		Scan(&name, &email, &cpf, &phone, &birth)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return a, err
	}
	a.name, a.email = ptrOrEmpty(name), ptrOrEmpty(email)
	a.cpf, a.phone = ptrOrEmpty(cpf), ptrOrEmpty(phone)
	if birth != nil {
		a.birthDate = *birth
	}
	return a, nil
}

type resetPasswordReq struct {
	Email    string `json:"email"`
	Code     string `json:"code"`
	Password string `json:"password"`
}

// resetPassword troca a senha com o código enviado por e-mail. Entrar pelo código sem
// trocar a senha deixava a pessoa presa num ciclo: toda volta dependia de outro e-mail.
// Provar posse do endereço é o que autoriza a troca — a senha antiga não é pedida
// justamente porque quem chega aqui não a tem.
func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	var body resetPasswordReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if len(body.Password) < minPasswordLen {
		writeErr(w, http.StatusBadRequest, "a senha precisa de ao menos 8 caracteres")
		return
	}
	email := normalizeEmail(body.Email)
	ctx := r.Context()
	if !s.consumeOTP(ctx, email, strings.TrimSpace(body.Code)) {
		writeErr(w, http.StatusUnauthorized, "código inválido ou expirado")
		return
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "erro ao salvar a senha")
		return
	}
	// O código veio para este endereço, então a conta existe e o e-mail está provado.
	var subjectID uuid.UUID
	if err := s.pool.QueryRow(ctx, `
		UPDATE subjects SET password_hash=$2, email_verified_at=COALESCE(email_verified_at, now()), updated_at=now()
		 WHERE lower(email)=$1 RETURNING id`, email, hash).Scan(&subjectID); err != nil {
		// Conta inexistente devolve o mesmo erro do código errado: dizer "não há conta"
		// aqui entregaria quem é cadastrado a quem só tem o e-mail.
		writeErr(w, http.StatusUnauthorized, "código inválido ou expirado")
		return
	}
	tok, err := s.auth.IssueBuyer(subjectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "erro ao emitir sessão")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "subject_id": subjectID})
}

// onlyDigits limpa máscara de formulário (pontos, traços, parênteses).
func onlyDigits(s string) string { return digitsOnly.ReplaceAllString(s, "") }
