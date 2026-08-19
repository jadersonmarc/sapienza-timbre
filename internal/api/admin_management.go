package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/dash"
	"github.com/jadersonmarc/sapienza-timbre/internal/store"
)

// ── produtores ────────────────────────────────────────────────────────────────

// listAdminProducers lista os produtores (filtro opcional por status).
func (s *Server) listAdminProducers(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	prods, err := store.ListProducers(r.Context(), s.pool, r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"producers": prods})
}

// ── artistas (catálogo global) ────────────────────────────────────────────────

type artistReq struct {
	Name     string  `json:"name"`
	Bio      *string `json:"bio"`
	ImageURL *string `json:"image_url"`
	Category *string `json:"category"`
}

// listArtists lista o catálogo global (all=true inclui suspensos).
func (s *Server) listArtists(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	artists, err := store.ListArtists(r.Context(), s.pool, r.URL.Query().Get("all") == "true")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artists": artists})
}

// createArtist cria um artista no catálogo global (admin ou signup público).
func (s *Server) createArtist(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
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
	s.audit(r, claims, "artist.create", "artist", &a.ID, map[string]any{"name": a.Name})
	writeJSON(w, http.StatusCreated, a)
}

// patchArtist atualiza o perfil do artista (admin).
func (s *Server) patchArtist(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body artistReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if err := store.UpdateArtist(r.Context(), s.pool, id, strings.TrimSpace(body.Name), body.Bio, body.ImageURL, body.Category); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, claims, "artist.update", "artist", &id, map[string]any{"name": body.Name})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// setArtistStatus suspende/reativa um artista (moderação reativa).
func (s *Server) setArtistStatus(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := decode(w, r, &body); err != nil || (body.Status != "active" && body.Status != "suspended") {
		writeErr(w, http.StatusBadRequest, "status deve ser active ou suspended")
		return
	}
	if err := store.SetArtistStatus(r.Context(), s.pool, id, body.Status); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, claims, "artist.set_status", "artist", &id, map[string]any{"status": body.Status})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": body.Status})
}

// insertArtist cria o artista gerando o slug; colisão de slug ganha sufixo curto.
func (s *Server) insertArtist(ctx context.Context, name string, bio, imageURL, category *string) (store.Artist, error) {
	slug := slugify(name)
	a, err := store.CreateArtist(ctx, s.pool, name, slug, bio, imageURL, category)
	if err != nil && isUniqueViolation(err) {
		slug = slug + "-" + uuid.NewString()[:6]
		a, err = store.CreateArtist(ctx, s.pool, name, slug, bio, imageURL, category)
	}
	return a, err
}

// ── eventos (visão global) ────────────────────────────────────────────────────

// listAdminEvents lista eventos de todos os produtores (visão consolidada).
func (s *Server) listAdminEvents(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	evs, err := dash.PlatformEvents(r.Context(), s.pool)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs})
}

// ── moderação ─────────────────────────────────────────────────────────────────

type moderationFlagReq struct {
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	Reason     string `json:"reason"`
}

// createModerationFlag é a denúncia PÚBLICA (comprador/visitante). Moderação é
// reativa: nada nasce bloqueado; a denúncia entra na fila do /admin.
func (s *Server) createModerationFlag(w http.ResponseWriter, r *http.Request) {
	var body moderationFlagReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if body.TargetType != "event" && body.TargetType != "artist" && body.TargetType != "producer" && body.TargetType != "buyer" {
		writeErr(w, http.StatusBadRequest, "target_type inválido")
		return
	}
	targetID, err := uuid.Parse(body.TargetID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "target_id inválido")
		return
	}
	if strings.TrimSpace(body.Reason) == "" {
		writeErr(w, http.StatusBadRequest, "reason obrigatório")
		return
	}
	flag, err := store.CreateModerationFlag(r.Context(), s.pool, body.TargetType, targetID, strings.TrimSpace(body.Reason))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, flag)
}

// moderationQueue lista a fila de denúncias (status pendente por padrão).
func (s *Server) moderationQueue(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	status := r.URL.Query().Get("status")
	flags, err := store.ListModerationFlags(r.Context(), s.pool, status)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"flags": flags})
}

// resolveModeration resolve uma denúncia (resolved ou dismissed) marcando o admin.
func (s *Server) resolveModeration(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := decode(w, r, &body); err != nil || (body.Status != "resolved" && body.Status != "dismissed") {
		writeErr(w, http.StatusBadRequest, "status deve ser resolved ou dismissed")
		return
	}
	adminID, err := claims.AdminID()
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "token inválido")
		return
	}
	if err := store.ResolveModerationFlag(r.Context(), s.pool, id, body.Status, adminID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, claims, "moderation.resolve", "moderation_flag", &id, map[string]any{"status": body.Status})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": body.Status})
}

// ── auditoria ─────────────────────────────────────────────────────────────────

// auditLog lista a trilha de ações administrativas (limit via query, default 100).
func (s *Server) auditLog(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	entries, err := store.ListAuditLog(r.Context(), s.pool, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// ── relatórios ────────────────────────────────────────────────────────────────

// reportsSales consolida as vendas da plataforma (relatório financeiro).
func (s *Server) reportsSales(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	sum, err := dash.SalesPlatform(r.Context(), s.pool)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// ── gestão de admins (super_admin) ────────────────────────────────────────────

// listAdmins lista os operadores da plataforma.
func (s *Server) listAdmins(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	admins, err := store.ListAdmins(r.Context(), s.pool)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"admins": admins})
}

type createAdminReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

// createAdmin cria um operador (super_admin only).
func (s *Server) createAdmin(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	var body createAdminReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	email := normalizeEmail(body.Email)
	if !looksLikeEmail(email) || len(body.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "email e password (mín. 8) obrigatórios")
		return
	}
	role := auth.RoleAdmin
	if body.Role != "" {
		if body.Role != auth.RoleAdmin && body.Role != auth.RoleSuperAdmin {
			writeErr(w, http.StatusBadRequest, "role inválida")
			return
		}
		role = body.Role
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	admin, err := store.CreateAdmin(r.Context(), s.pool, email, hash, role)
	if err != nil {
		if isUniqueViolation(err) {
			writeErr(w, http.StatusConflict, "e-mail já cadastrado")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, claims, "admin.create", "admin", &admin.ID, map[string]any{"email": email, "role": role})
	writeJSON(w, http.StatusCreated, admin)
}

// setAdminRole muda o papel de um operador (super_admin only).
func (s *Server) setAdminRole(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := decode(w, r, &body); err != nil || (body.Role != auth.RoleAdmin && body.Role != auth.RoleSuperAdmin) {
		writeErr(w, http.StatusBadRequest, "role inválida")
		return
	}
	if err := store.SetAdminRole(r.Context(), s.pool, id, body.Role); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, claims, "admin.set_role", "admin", &id, map[string]any{"role": body.Role})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "role": body.Role})
}

// ── helpers ──────────────────────────────────────────────────────────────────

// slugify converte um nome em slug (a-z0-9 e hífen). Acentos viram hífen; nomes vazios
// caem em "artista" — o catálogo global só precisa de um identificador estável.
func slugify(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if s == "" {
		s = "artista"
	}
	return s
}

func atoi(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("não numérico")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}
