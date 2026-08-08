package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/transfer"
)

type transferReq struct {
	ToWalletID string `json:"to_wallet_id"`
	PriceCents int64  `json:"price_cents"`
}

// transferTicket faz a transferência restrita de um ingresso (com teto e royalty). Por
// ora é operada pelo produtor (owner); a transferência iniciada pelo comprador, com a
// carteira dele, entra junto com a identidade/mercado secundário.
func (s *Server) transferTicket(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	ticketID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body transferReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	toWallet, err := uuid.Parse(body.ToWalletID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "to_wallet_id inválido")
		return
	}
	var res transfer.Result
	err = s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		res, e = transfer.Execute(r.Context(), tx, ticketID, toWallet, body.PriceCents, s.seams.Chain.Enabled())
		return e
	})
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, res)
	case errors.Is(err, transfer.ErrPriceCap):
		writeErr(w, http.StatusBadRequest, "preço acima do teto de revenda")
	case errors.Is(err, transfer.ErrNotActive), errors.Is(err, transfer.ErrNotTransferable), errors.Is(err, transfer.ErrDisputed):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}
