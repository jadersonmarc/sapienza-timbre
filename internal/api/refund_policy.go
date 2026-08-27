package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/checkout"
)

// policyRef resolve qual política a rota edita: a do evento (com {id} na rota) ou o default
// do produtor. Devolve nil para o default, que é como a camada de dados o representa.
func policyRef(r *http.Request) (*uuid.UUID, error) {
	raw := r.PathValue("id")
	if raw == "" {
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// getRefundPolicy devolve a política GRAVADA (sem herança) — é o que o formulário edita.
// `configured:false` diz que o evento ainda não decidiu nada e está seguindo o default.
func (s *Server) getRefundPolicy(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	ref, err := policyRef(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "evento inválido")
		return
	}
	var p checkout.Policy
	var configured bool
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		p, configured, e = checkout.GetPolicy(r.Context(), tx, ref)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"policy": p, "configured": configured,
		"legal_minimum_days": checkout.LegalWithdrawalDays,
	})
}

// putRefundPolicy grava a política do evento ou o default do produtor.
func (s *Server) putRefundPolicy(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	ref, err := policyRef(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "evento inválido")
		return
	}
	// Parte do default para que um campo omitido não vire zero: janela zero seria ilegal e
	// prazo de resposta zero travaria a fila.
	p := checkout.DefaultPolicy()
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		return checkout.SavePolicy(r.Context(), tx, ref, p)
	}); err != nil {
		writeCheckoutErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": p})
}

// publicRefundPolicy é a promessa como o comprador precisa ler ANTES de comprar e antes de
// pedir devolução. Sem auth: é informação de venda, não dado de operação.
func (s *Server) publicRefundPolicy(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "evento inválido")
		return
	}
	producerID, err := s.producerOfEvent(r.Context(), eventID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "evento não encontrado")
		return
	}
	var p checkout.Policy
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		var e error
		p, e = checkout.ResolvePolicy(r.Context(), tx, eventID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// O comprador não precisa saber quem come a tarifa nem o prazo interno de resposta do
	// produtor: são acertos entre a casa e a plataforma. O que é dele é o que pode pedir.
	writeJSON(w, http.StatusOK, map[string]any{
		"withdrawal_window_days":            p.WithdrawalWindowDays,
		"withdrawal_min_hours_before_event": p.WithdrawalMinHoursBeforeEvent,
		"accepts_requests_after_window":     p.ProducerDiscretionaryEnabled,
		"checkin_blocks_refund":             p.CheckinBlocksRefund,
	})
}
