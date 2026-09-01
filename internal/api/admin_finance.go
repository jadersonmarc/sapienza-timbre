package api

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/checkout"
)

// adminLineup mostra e edita o rateio informativo do line-up de um evento. Fica no admin
// porque hoje é daqui que a operação acompanha os eventos — e o rateio não move dinheiro,
// então editá-lo não é ato financeiro.
func (s *Server) adminLineup(w http.ResponseWriter, r *http.Request, _ *auth.AdminClaims) {
	producerID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "produtor inválido")
		return
	}
	eventID, err := uuid.Parse(r.PathValue("eventId"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "evento inválido")
		return
	}
	if r.Method == http.MethodGet {
		var shares []checkout.LineupShare
		if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
			var e error
			shares, e = checkout.LineupPreview(r.Context(), tx, eventID)
			return e
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if shares == nil {
			shares = []checkout.LineupShare{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"shares": shares})
		return
	}

	var body lineupReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	var total float64
	for _, sh := range body.Shares {
		total += sh.SharePct
	}
	if total > 100 {
		writeErr(w, http.StatusBadRequest, "a soma do line-up passa de 100% do valor de face")
		return
	}
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		if _, e := tx.Exec(r.Context(), `DELETE FROM lineup_shares WHERE event_id=$1`, eventID); e != nil {
			return e
		}
		for _, sh := range body.Shares {
			if _, e := tx.Exec(r.Context(), `
				INSERT INTO lineup_shares (event_id, artist_name, share_pct) VALUES ($1,$2,$3)`,
				eventID, sh.ArtistName, sh.SharePct); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
