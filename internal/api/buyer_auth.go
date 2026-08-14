package api

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/notify"
)

// Constantes do OTP do comprador — PROVISÓRIAS, isoladas. Sem definição de produto, valores
// conservadores; reportar para calibrar.
const (
	otpTTL            = 10 * time.Minute // validade do código
	otpLength         = 6                // dígitos
	otpMaxAttempts    = 5                // tentativas de verificação por código
	otpResendCooldown = 60 * time.Second // intervalo mínimo entre reenvios ao mesmo e-mail
	otpMaxPerHour     = 6                // teto de códigos por e-mail por hora
)

// buyerHandler é um handler já resolvido para o subject do comprador autenticado.
type buyerHandler func(w http.ResponseWriter, r *http.Request, subjectID uuid.UUID)

// buyerAuthed valida o token do comprador (escopo "buyer") e injeta o subject id. Escopo
// separado do produtor (§4.1): um token de produtor não passa aqui.
func (s *Server) buyerAuthed(fn buyerHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		subjectID, err := s.auth.VerifyBuyer(tok)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "invalid token")
			return
		}
		fn(w, r, subjectID)
	}
}

type requestCodeReq struct {
	Email string `json:"email"`
}

// requestCode gera e envia um código de acesso. Resposta SEMPRE genérica e idêntica —
// nunca revela se o e-mail já tem conta (§4.1). Cooldown e teto por hora contra abuso.
func (s *Server) requestCode(w http.ResponseWriter, r *http.Request) {
	var body requestCodeReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	email := normalizeEmail(body.Email)
	// Resposta neutra usada em todos os caminhos (válido, inválido, em cooldown).
	neutral := func() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Se o e-mail for válido, enviamos um código."})
	}
	if !looksLikeEmail(email) {
		neutral()
		return
	}
	ctx := r.Context()

	// Teto por hora e cooldown — silenciosos (não revelam nada ao chamador).
	var recent, sinceLast int
	if err := s.pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE created_at > now() - interval '1 hour'),
		  count(*) FILTER (WHERE created_at > now() - make_interval(secs => $2))
		FROM buyer_otps WHERE lower(email) = $1`, email, otpResendCooldown.Seconds()).Scan(&recent, &sinceLast); err != nil {
		neutral()
		return
	}
	if recent >= otpMaxPerHour || sinceLast > 0 {
		neutral()
		return
	}

	code := numericCode(otpLength)
	hash, err := auth.HashPassword(code)
	if err != nil {
		neutral()
		return
	}
	// Invalida códigos anteriores não consumidos (só o último vale) e grava o novo.
	if _, err := s.pool.Exec(ctx, `
		UPDATE buyer_otps SET consumed_at = now()
		 WHERE lower(email) = $1 AND consumed_at IS NULL`, email); err != nil {
		neutral()
		return
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO buyer_otps (email, code_hash, expires_at, requested_ip)
		VALUES ($1, $2, now() + make_interval(secs => $3), $4)`,
		email, hash, otpTTL.Seconds(), clientIP(r)); err != nil {
		neutral()
		return
	}
	_ = s.seams.Notify.Send(ctx, notify.Message{
		Channel: "email", To: email, Subject: "Seu código de acesso Timbre",
		Body: fmt.Sprintf("Seu código de acesso é %s. Ele vale por %d minutos.", code, int(otpTTL.Minutes())),
	})
	neutral()
}

type verifyCodeReq struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

// verifyCode confere o código, resolve/cria o subject e — SÓ APÓS verificar — vincula
// as compras feitas como convidado com o mesmo e-mail (§3.4). Devolve o token do comprador.
func (s *Server) verifyCode(w http.ResponseWriter, r *http.Request) {
	var body verifyCodeReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	email := normalizeEmail(body.Email)
	code := strings.TrimSpace(body.Code)
	ctx := r.Context()

	var otpID uuid.UUID
	var hash string
	var attempts int
	err := s.pool.QueryRow(ctx, `
		SELECT id, code_hash, attempts FROM buyer_otps
		 WHERE lower(email) = $1 AND consumed_at IS NULL AND expires_at > now()
		 ORDER BY created_at DESC LIMIT 1`, email).Scan(&otpID, &hash, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusUnauthorized, "código inválido ou expirado")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "erro ao verificar")
		return
	}
	if attempts >= otpMaxAttempts {
		writeErr(w, http.StatusTooManyRequests, "muitas tentativas; solicite um novo código")
		return
	}
	if !auth.ComparePassword(hash, code) {
		_, _ = s.pool.Exec(ctx, `UPDATE buyer_otps SET attempts = attempts + 1 WHERE id = $1`, otpID)
		writeErr(w, http.StatusUnauthorized, "código inválido ou expirado")
		return
	}
	// Sucesso: consome o código, resolve o subject e vincula as compras de convidado.
	if _, err := s.pool.Exec(ctx, `UPDATE buyer_otps SET consumed_at = now() WHERE id = $1`, otpID); err != nil {
		writeErr(w, http.StatusInternalServerError, "erro ao verificar")
		return
	}
	subjectID, err := s.resolveSubjectByEmail(ctx, email)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "erro ao verificar")
		return
	}
	// Vínculo retroativo SÓ por e-mail verificado (§3.4).
	if _, err := s.pool.Exec(ctx, `
		UPDATE ticket_directory SET subject_id = $1
		 WHERE subject_id IS NULL AND lower(buyer_email) = $2`, subjectID, email); err != nil {
		writeErr(w, http.StatusInternalServerError, "erro ao verificar")
		return
	}
	tok, err := s.auth.IssueBuyer(subjectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "erro ao emitir sessão")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "subject_id": subjectID})
}

// resolveSubjectByEmail acha o subject por e-mail (case-insensitive) ou cria um. subjects
// não tem unique em email (pode haver histórico); select-then-insert é suficiente aqui.
func (s *Server) resolveSubjectByEmail(ctx context.Context, email string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT id FROM subjects WHERE lower(email) = $1 ORDER BY created_at LIMIT 1`, email).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	if err := s.pool.QueryRow(ctx, `INSERT INTO subjects (email) VALUES ($1) RETURNING id`, email).Scan(&id); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func normalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func looksLikeEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	return at > 0 && at < len(s)-1 && strings.IndexByte(s[at+1:], '.') >= 0
}

// numericCode gera um código numérico de n dígitos com crypto/rand.
func numericCode(n int) string {
	const digits = "0123456789"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = digits[int(b[i])%len(digits)]
	}
	return string(b)
}

// clientIP resolve o IP do chamador (X-Forwarded-For do proxy, senão RemoteAddr).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
