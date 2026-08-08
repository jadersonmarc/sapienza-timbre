package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/nft"
)

// tokenMetadata devolve os metadados públicos ERC-1155 do token (renderável por carteira
// externa; sem dado pessoal). Resolve o produtor pelo índice público.
func (s *Server) tokenMetadata(w http.ResponseWriter, r *http.Request) {
	ticketID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var name string
	var description *string
	var attributes []byte
	err = s.pool.QueryRow(r.Context(), `SELECT name, description, attributes FROM public.token_metadata WHERE ticket_id=$1`, ticketID).
		Scan(&name, &description, &attributes)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "token não encontrado")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Formato ERC-1155: name, description, attributes (o jsonb entra cru).
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "description": description, "attributes": json.RawMessage(attributes),
	})
}

// tokenView é a prova de propriedade pública compartilhável: estado do token + resolução
// do produtor. Estado é calculado ao vivo no schema do produtor.
func (s *Server) tokenView(w http.ResponseWriter, r *http.Request) {
	ticketID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	producerID, err := s.producerOfToken(r.Context(), ticketID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "token não encontrado")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var state nft.State
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		var e error
		state, e = nft.TicketState(r.Context(), tx, ticketID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ticket_id": ticketID, "state": state})
}

func (s *Server) producerOfToken(ctx context.Context, ticketID uuid.UUID) (uuid.UUID, error) {
	var pid uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT producer_id FROM public.token_metadata WHERE ticket_id=$1`, ticketID).Scan(&pid)
	return pid, err
}

// exportTicket passa a custódia ao participante (carteira externa). Operado pelo owner
// nesta etapa; a autonomia do participante entra com a identidade.
func (s *Server) exportTicket(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	ticketID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		return nft.ExportTicket(r.Context(), tx, ticketID)
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "custody": "external"})
}

type disputeReq struct {
	Reason string `json:"reason"`
}

// disputeTicket abre uma disputa: bloqueia transferência, não a entrada.
func (s *Server) disputeTicket(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	ticketID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body disputeReq
	_ = decode(w, r, &body)
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		return nft.OpenDispute(r.Context(), tx, ticketID, body.Reason)
	}); err != nil {
		if errors.Is(err, nft.ErrAlreadyDisputed) {
			writeErr(w, http.StatusConflict, "ingresso já em disputa")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// reissueTicket queima o ingresso atual e emite um novo assinado (perda de acesso).
func (s *Server) reissueTicket(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	ticketID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var newID uuid.UUID
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		newID, e = nft.Reissue(r.Context(), tx, s.signer, claims.ProducerID, ticketID)
		return e
	}); err != nil {
		if errors.Is(err, nft.ErrNotReissuable) {
			writeErr(w, http.StatusConflict, "ingresso não reemitível")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ticket_id": newID})
}
