package api

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/dash"
)

// adminSetProducerStatus suspende/reativa um produtor (plataforma). Aprovação manual
// virou exceção: o cadastro público nasce ativo (self-service); esta rota fica para
// reativar um produtor suspenso.
func (s *Server) adminSetProducerStatus(status string) adminHandler {
	return func(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
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
		s.audit(r, claims, "producer.set_status", "producer", &id, map[string]any{"status": status})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status})
	}
}

// adminSuspendEvent suspende um evento no schema do produtor (moderação).
func (s *Server) adminSuspendEvent(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
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
	s.audit(r, claims, "event.suspend", "event", &eventID, map[string]any{"producer_id": producerID})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// adminSummary consolida a plataforma (produtores, eventos ativos, faturamento do dia).
func (s *Server) adminSummary(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	sum, err := dash.Platform(r.Context(), s.pool)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	funnel, err := dash.PlatformSessionFunnel(r.Context(), s.pool)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"producers_total": sum.ProducersTotal, "producers_pending": sum.ProducersPending,
		"events_active": sum.EventsActive, "revenue_today_cents": sum.RevenueTodayCents,
		"session_funnel": funnel,
	})
}
