package api

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/checkout"
)

type lineupShareReq struct {
	ArtistName string  `json:"artist_name"`
	ArtistID   *string `json:"artist_id"`
	SharePct   float64 `json:"share_pct"`
}

type lineupReq struct {
	Shares []lineupShareReq `json:"shares"`
}

// setLineup grava o rateio do line-up do evento. É INFORMATIVO: alimenta o painel e não
// movimenta dinheiro — nenhum artista é recebedor no gateway, quem paga o artista é o
// produtor. Por isso a soma pode ficar abaixo de 100% (a diferença é do produtor), mas
// nunca acima: prometer mais do que entra seria conta que não fecha.
func (s *Server) setLineup(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body lineupReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	var total float64
	for i, sh := range body.Shares {
		if strings.TrimSpace(sh.ArtistName) == "" {
			writeErr(w, http.StatusBadRequest, "informe o nome do artista")
			return
		}
		if sh.SharePct < 0 || sh.SharePct > 100 {
			writeErr(w, http.StatusBadRequest, "percentual inválido no artista "+sh.ArtistName)
			return
		}
		total += sh.SharePct
		_ = i
	}
	if total > 100 {
		writeErr(w, http.StatusBadRequest, "a soma do line-up passa de 100% do valor de face")
		return
	}
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		if _, e := tx.Exec(r.Context(), `DELETE FROM lineup_shares WHERE event_id=$1`, eventID); e != nil {
			return e
		}
		for _, sh := range body.Shares {
			var artistID *uuid.UUID
			if sh.ArtistID != nil && *sh.ArtistID != "" {
				if id, e := uuid.Parse(*sh.ArtistID); e == nil {
					artistID = &id
				}
			}
			if _, e := tx.Exec(r.Context(), `
				INSERT INTO lineup_shares (event_id, artist_id, artist_name, share_pct)
				VALUES ($1,$2,$3,$4)`,
				eventID, artistID, strings.TrimSpace(sh.ArtistName), sh.SharePct); e != nil {
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

// getLineup devolve o rateio com o valor previsto sobre o face já vendido. Previsto, não
// devido: é o produtor quem paga o artista, por fora da plataforma.
func (s *Server) getLineup(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var shares []checkout.LineupShare
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
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
	writeJSON(w, http.StatusOK, map[string]any{
		"shares": shares,
		"nota":   "Valores previstos sobre o face vendido. O pagamento aos artistas é feito pelo produtor.",
	})
}
