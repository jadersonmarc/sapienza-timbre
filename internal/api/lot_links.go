package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/catalog"
)

type lotLinkReq struct {
	// Label é o apelido do link — "lista do parceiro", "pré-venda fã-clube". Quem cria três
	// links não distingue um do outro pelo token.
	Label string `json:"label"`
	// MaxUses nulo = sem limite. Contado em INGRESSOS confirmados.
	MaxUses *int `json:"max_uses"`
	// ExpiresAt nulo = sem validade.
	ExpiresAt *string `json:"expires_at"`
}

// createLotLink cria o link exclusivo de uma categoria e a torna oculta.
//
// Criar o link ESCONDE a categoria da página pública, de propósito: link privado para algo
// que já aparece na página não é privado — e deixar as duas coisas separadas garantiria que,
// mais cedo ou mais tarde, alguém criasse o link e esquecesse de esconder.
func (s *Server) createLotLink(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	lotID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body lotLinkReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	var expires *time.Time
	if body.ExpiresAt != nil && *body.ExpiresAt != "" {
		t, e := time.Parse(time.RFC3339, *body.ExpiresAt)
		if e != nil {
			writeErr(w, http.StatusBadRequest, "expires_at inválido (use RFC3339)")
			return
		}
		expires = &t
	}
	var out catalog.LotLink
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = catalog.CreateLotLink(r.Context(), tx, lotID, body.Label, body.MaxUses, expires)
		return e
	}); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// listLotLinks lista os links das categorias do evento, com o estado de cada um.
func (s *Server) listLotLinks(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var out []catalog.LotLink
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = catalog.ListLotLinks(r.Context(), tx, eventID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": out})
}

// revokeLotLink desliga o link. A validade é conferida a cada uso, então ele para de
// funcionar na hora.
func (s *Server) revokeLotLink(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		return catalog.RevokeLotLink(r.Context(), tx, id)
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// publicLotByLink resolve a categoria oculta que um token abre. É o que a página do link
// chama antes de mostrar o botão de compra — e o que responde 404 quando o link foi
// revogado, venceu ou esgotou.
//
// Os quatro casos devolvem a MESMA resposta: distinguir "não existe" de "foi revogado" para
// quem só tem o token é entregar informação a quem está tentando adivinhar.
func (s *Server) publicLotByLink(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	producerID, err := s.producerOfEvent(r.Context(), eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "evento não encontrado")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	token := r.URL.Query().Get("k")
	var lot catalog.Lot
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		l, _, e := catalog.ResolveLotLink(r.Context(), tx, eventID, token)
		lot = l
		return e
	}); err != nil {
		if errors.Is(err, catalog.ErrLinkInvalid) {
			writeErr(w, http.StatusNotFound, "este link não está mais disponível")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lot": publicLot{
		ID: lot.ID, Name: lot.Name, PriceCents: lot.PriceCents,
		Available: lot.Quantity - lot.SoldCount - lot.HeldCount,
		StartsAt:  lot.StartsAt, EndsAt: lot.EndsAt, SortOrder: lot.SortOrder,
		MinPurchaseQuantity: lot.MinPurchaseQuantity, MaxPurchaseQuantity: lot.MaxPurchaseQuantity,
		Notice: lot.Notice, OnSale: true,
	}})
}
