package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/market"
	"github.com/jadersonmarc/sapienza-timbre/internal/transfer"
)

type createListingReq struct {
	PriceCents int64 `json:"price_cents"`
}

// createListing anuncia um ingresso para revenda (owner por ora).
func (s *Server) createListing(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	ticketID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body createListingReq
	if err := decode(w, r, &body); err != nil || body.PriceCents <= 0 {
		writeErr(w, http.StatusBadRequest, "price_cents obrigatório (> 0)")
		return
	}
	var out market.Listing
	err = s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = market.CreateListing(r.Context(), tx, claims.ProducerID, ticketID, body.PriceCents)
		return e
	})
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, out)
	case errors.Is(err, transfer.ErrPriceCap):
		writeErr(w, http.StatusBadRequest, "preço acima do teto de revenda")
	case errors.Is(err, transfer.ErrNotActive), errors.Is(err, transfer.ErrNotTransferable), errors.Is(err, market.ErrAlreadyListed):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

// cancelListing cancela um anúncio (owner).
func (s *Server) cancelListing(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	listingID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		return market.CancelListing(r.Context(), tx, listingID)
	}); err != nil {
		if errors.Is(err, market.ErrListingUnavailable) {
			writeErr(w, http.StatusConflict, "anúncio indisponível")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// provenance devolve a procedência de um ingresso (owner/relatorios).
func (s *Server) provenance(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	ticketID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var p market.Provenance
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		p, e = market.GetProvenance(r.Context(), tx, ticketID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

type buyListingReq struct {
	BuyerEmail string `json:"buyer_email"`
}

// buyListing é a compra de um anúncio pelo comprador AUTENTICADO (cadastro obrigatório).
// Resolve o produtor pelo índice público e cria a cobrança; a titularidade só passa na
// confirmação do pagamento (webhook).
func (s *Server) buyListing(w http.ResponseWriter, r *http.Request, subjectID uuid.UUID) {
	listingID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body buyListingReq
	_ = decode(w, r, &body)
	email, err := s.buyerEmail(r.Context(), subjectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	producerID, err := s.producerOfListing(r.Context(), listingID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "anúncio não encontrado")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var res market.BuyResult
	err = s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		var e error
		res, e = market.BuyListing(r.Context(), tx, s.seams.Payment, producerID, listingID, email)
		return e
	})
	if errors.Is(err, market.ErrListingUnavailable) {
		writeErr(w, http.StatusConflict, "anúncio indisponível")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) producerOfListing(ctx context.Context, listingID uuid.UUID) (uuid.UUID, error) {
	var pid uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT producer_id FROM public.listing_index WHERE listing_id=$1`, listingID).Scan(&pid)
	return pid, err
}
