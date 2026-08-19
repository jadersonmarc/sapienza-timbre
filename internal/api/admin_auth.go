package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/store"
)

// adminHandler é um handler já resolvido para o admin autenticado (escopo "admin").
type adminHandler func(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims)

// requireAdmin valida o JWT de admin e injeta as claims. Escopo separado do produtor
// (§4.1): um token de colaborador/comprador não passa aqui. Substitui o antigo
// X-Admin-Token, que ficou restrito ao bootstrap (POST /producers).
func (s *Server) requireAdmin(fn adminHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		claims, err := s.auth.VerifyAdmin(tok)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "invalid token")
			return
		}
		fn(w, r, claims)
	}
}

// requireSuperAdmin exige papel super_admin (gestão de admins e decisões globais).
func (s *Server) requireSuperAdmin(fn adminHandler) http.HandlerFunc {
	return s.requireAdmin(func(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
		if !claims.IsSuperAdmin() {
			writeErr(w, http.StatusForbidden, "requer super_admin")
			return
		}
		fn(w, r, claims)
	})
}

type adminLoginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// adminLogin autentica um operador da plataforma e devolve o JWT de admin (escopo
// "admin"). O primeiro super_admin é semeado no boot via TIMBRE_ADMIN_EMAIL/PASSWORD.
func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	var body adminLoginReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	email := normalizeEmail(body.Email)
	admin, err := store.FindAdminByEmail(r.Context(), s.pool, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusUnauthorized, "credenciais inválidas")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !auth.ComparePassword(admin.PasswordHash, body.Password) {
		writeErr(w, http.StatusUnauthorized, "credenciais inválidas")
		return
	}
	tok, err := s.auth.IssueAdmin(admin.ID, admin.Role, admin.SessionVersion)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "role": admin.Role})
}

// adminMe devolve o admin autenticado (prova a sessão do painel).
func (s *Server) adminMe(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	adminID, err := claims.AdminID()
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "token inválido")
		return
	}
	admin, err := store.GetAdmin(r.Context(), s.pool, adminID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, admin)
}

// audit grava uma ação administrativa na trilha (append-only).
func (s *Server) audit(r *http.Request, claims *auth.AdminClaims, action, entityType string, entityID *uuid.UUID, details map[string]any) {
	var adminID *uuid.UUID
	if claims != nil {
		if id, err := claims.AdminID(); err == nil {
			adminID = &id
		}
	}
	_ = store.AppendAudit(r.Context(), s.pool, adminID, action, strPtr(entityType), entityID, details)
}

// strPtr devolve nil para string vazia (colunas nullable do audit_log).
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
