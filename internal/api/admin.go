package api

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/dash"
)

// requireAdmin protege as rotas de plataforma com o X-Admin-Token (o operador da
// Sapienza). Uma auth/UI de admin dedicada vem depois; por ora é o token de bootstrap.
func (s *Server) requireAdmin(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.adminToken == "" {
			writeErr(w, http.StatusServiceUnavailable, "admin desligado (defina TIMBRE_ADMIN_TOKEN)")
			return
		}
		if !subtleCompare(r.Header.Get("X-Admin-Token"), s.adminToken) {
			writeErr(w, http.StatusUnauthorized, "token de admin inválido")
			return
		}
		fn(w, r)
	}
}

func (s *Server) adminSetProducerStatus(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "id inválido")
			return
		}
		tag, err := s.pool.Exec(r.Context(), `UPDATE public.producers SET status=$2, updated_at=now() WHERE id=$1`, id, status)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if tag.RowsAffected() == 0 {
			writeErr(w, http.StatusNotFound, "produtor não encontrado")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status})
	}
}

// adminSuspendEvent suspende um evento no schema do produtor.
func (s *Server) adminSuspendEvent(w http.ResponseWriter, r *http.Request) {
	producerID, err := uuid.Parse(r.PathValue("pid"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "pid inválido")
		return
	}
	eventID, err := uuid.Parse(r.PathValue("eid"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "eid inválido")
		return
	}
	var affected int64
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		tag, e := tx.Exec(r.Context(), `UPDATE events SET status='suspended', updated_at=now() WHERE id=$1`, eventID)
		if e != nil {
			return e
		}
		affected = tag.RowsAffected()
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if affected == 0 {
		writeErr(w, http.StatusNotFound, "evento não encontrado")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// adminSummary consolida a plataforma (produtores, eventos ativos, faturamento do dia).
func (s *Server) adminSummary(w http.ResponseWriter, r *http.Request) {
	sum, err := dash.Platform(r.Context(), s.pool)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sum)
}
