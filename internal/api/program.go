package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/program"
)

type setTierReq struct {
	Tier          string  `json:"tier"`
	EffectiveFrom *string `json:"effective_from"`
}

// adminSetTier registra a transição de nível do produtor (admin). A apuração passada
// não muda — sempre usa o nível vigente na data da venda.
func (s *Server) adminSetTier(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body setTierReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	eff := time.Now()
	if body.EffectiveFrom != nil && *body.EffectiveFrom != "" {
		if t, e := time.Parse(time.RFC3339, *body.EffectiveFrom); e == nil {
			eff = t
		} else {
			writeErr(w, http.StatusBadRequest, "effective_from inválido (RFC3339)")
			return
		}
	}
	if err := program.SetTier(r.Context(), s.pool, id, body.Tier, eff); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tier": body.Tier})
}

type setOriginationReq struct {
	OriginatorID     string   `json:"originator_id"`
	ParticipationPct *float64 `json:"participation_pct"`
	EffectiveUntil   *string  `json:"effective_until"`
}

// adminSetOrigination registra que um produtor foi indicado por um originador (admin).
// participation_pct é PROVISÓRIO (default 0 → inerte) até definição comercial.
func (s *Server) adminSetOrigination(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body setOriginationReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	originatorID, err := uuid.Parse(body.OriginatorID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "originator_id inválido")
		return
	}
	pct := program.DefaultOriginatorPct
	if body.ParticipationPct != nil {
		pct = *body.ParticipationPct
	}
	var until *time.Time
	if body.EffectiveUntil != nil && *body.EffectiveUntil != "" {
		t, e := time.Parse(time.RFC3339, *body.EffectiveUntil)
		if e != nil {
			writeErr(w, http.StatusBadRequest, "effective_until inválido (RFC3339)")
			return
		}
		until = &t
	}
	if err := program.SetOrigination(r.Context(), s.pool, id, originatorID, pct, until); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// dashProgram mostra o nível vigente e os percentuais aplicáveis ao produtor.
func (s *Server) dashProgram(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	ap, err := program.Apurar(r.Context(), s.pool, claims.ProducerID, 0, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tier": ap.Tier, "fee_pct": ap.FeePct, "tier_pct": ap.TierPct})
}

// dashOrigination é o extrato do originador: o que ele apurou por produtores indicados.
func (s *Server) dashOrigination(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT producer_id::text, event_id::text, amount_cents, to_char(created_at,'YYYY-MM-DD"T"HH24:MI:SS')
		  FROM public.origination_entries WHERE originator_producer_id=$1 ORDER BY created_at DESC LIMIT 200`, claims.ProducerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	type entry struct {
		ProducerID string `json:"producer_id"`
		EventID    string `json:"event_id"`
		Amount     int64  `json:"amount_cents"`
		At         string `json:"at"`
	}
	var out []entry
	var total int64
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.ProducerID, &e.EventID, &e.Amount, &e.At); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		total += e.Amount
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_cents": total, "entries": out})
}
