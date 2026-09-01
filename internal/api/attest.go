package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/attest"
	"github.com/jadersonmarc/sapienza-timbre/internal/audit"
	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/chain"
)

// ── categorias de cortesia (por produtor) ────────────────────────────────────

// listCourtesyCategories lista as categorias do produtor.
func (s *Server) listCourtesyCategories(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	var cats []attest.Category
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		cats, e = attest.ListCategories(r.Context(), tx, r.URL.Query().Get("all") == "true")
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": cats})
}

type categoryReq struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	SortOrder *int   `json:"sort_order"`
	Active    *bool  `json:"active"`
}

// createCourtesyCategory cria uma categoria (owner).
func (s *Server) createCourtesyCategory(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	var body categoryReq
	if err := decode(w, r, &body); err != nil || body.Slug == "" || body.Name == "" {
		writeErr(w, http.StatusBadRequest, "slug e name obrigatórios")
		return
	}
	order := 0
	if body.SortOrder != nil {
		order = *body.SortOrder
	}
	var cat attest.Category
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		cat, e = attest.CreateCategory(r.Context(), tx, body.Slug, body.Name, order)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, cat)
}

// patchCourtesyCategory atualiza uma categoria (owner).
func (s *Server) patchCourtesyCategory(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body categoryReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	// Só o que veio no corpo muda: arquivar manda `active` sozinho.
	var name *string
	if body.Name != "" {
		name = &body.Name
	}
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		return attest.UpdateCategory(r.Context(), tx, id, name, body.SortOrder, body.Active)
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ── compromissos declarados ──────────────────────────────────────────────────

type commitmentReq struct {
	Kind               string  `json:"kind"`
	CourtesyCategoryID *string `json:"courtesy_category_id"`
	TargetType         string  `json:"target_type"`
	TargetValue        string  `json:"target_value"`
	Description        *string `json:"description"`
}

// createCommitment declara um compromisso (owner). Bloqueado após o fechamento.
func (s *Server) createCommitment(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body commitmentReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	var catID *uuid.UUID
	if body.CourtesyCategoryID != nil {
		id, e := uuid.Parse(*body.CourtesyCategoryID)
		if e != nil {
			writeErr(w, http.StatusBadRequest, "courtesy_category_id inválido")
			return
		}
		catID = &id
	}
	var out attest.Commitment
	err = s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = attest.CreateCommitment(r.Context(), tx, attest.Commitment{
			EventID: eventID, Kind: body.Kind, CourtesyCategoryID: catID,
			TargetType: body.TargetType, TargetValue: body.TargetValue, Description: body.Description,
		})
		return e
	})
	switch {
	case errors.Is(err, attest.ErrClosed):
		writeErr(w, http.StatusConflict, "evento já fechado")
	case errors.Is(err, attest.ErrCommitmentOverflow):
		writeErr(w, http.StatusBadRequest, "soma dos percentuais excede 100%")
	case errors.Is(err, attest.ErrCommitmentMissingCategory):
		writeErr(w, http.StatusBadRequest, "courtesy_share exige categoria")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusCreated, out)
	}
}

// listCommitments lista os compromissos do evento.
func (s *Server) listCommitments(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var comms []attest.Commitment
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		comms, e = attest.ListCommitments(r.Context(), tx, eventID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"commitments": comms})
}

// deleteCommitment remove um compromisso (owner). Bloqueado após o fechamento.
func (s *Server) deleteCommitment(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	err = s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		return attest.DeleteCommitment(r.Context(), tx, id)
	})
	switch {
	case errors.Is(err, attest.ErrClosed):
		writeErr(w, http.StatusConflict, "evento já fechado")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// ── fechamento ───────────────────────────────────────────────────────────────

// closeEvent fecha o evento e gera/atualiza o atestado (owner).
func (s *Server) closeEvent(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var a attest.Attestation
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		a, e = attest.Close(r.Context(), tx, s.attest, s.anchorer, s.anchorMode, s.attestKeyID, claims.ProducerID, eventID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": a.ID, "version": a.Version, "supersedes_id": a.SupersedesID,
		"anchor_status": a.AnchorStatus, "closed_at": a.ClosedAt,
	})
}

// reanchorEvent reenfileira a âncora de um atestado (reancoragem manual). Só aceita
// atestado com anchor_status 'failed' ou 'none'; zera as tentativas do job.
func (s *Server) reanchorEvent(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	attID, err := uuid.Parse(r.PathValue("attId"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "attestation id inválido")
		return
	}
	var status string
	err = s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		if e := tx.QueryRow(r.Context(), `SELECT anchor_status FROM event_attestations WHERE id=$1 AND event_id=$2`, attID, eventID).Scan(&status); e != nil {
			return e
		}
		if status != "failed" && status != "none" {
			return errAnchorNotReanchorable
		}
		return chain.EnqueueAnchor(r.Context(), tx, attID)
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeErr(w, http.StatusNotFound, "atestado não encontrado")
	case errors.Is(err, errAnchorNotReanchorable):
		writeErr(w, http.StatusConflict, "atestado não está em estado reancorável (failed ou none)")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "anchor_status": "pending"})
	}
}

var errAnchorNotReanchorable = errors.New("attest: atestado não reancorável neste estado")

// ── relatórios ───────────────────────────────────────────────────────────────

// recordFor devolve o registro (do atestado vigente, ou provisório ao vivo).
func (s *Server) recordFor(ctx context.Context, tx pgx.Tx, producerID, eventID uuid.UUID) (attest.Record, *attest.Attestation, error) {
	a, err := attest.Current(ctx, tx, eventID)
	if err != nil {
		return attest.Record{}, nil, err
	}
	if a != nil {
		var rec attest.Record
		if err := json.Unmarshal(a.Payload, &rec); err != nil {
			return attest.Record{}, nil, err
		}
		return rec, a, nil
	}
	rec, err := attest.BuildRecord(ctx, tx, producerID, eventID)
	return rec, nil, err
}

// audienceReport é o relatório de público (deriva do atestado vigente; provisório antes).
func (s *Server) audienceReport(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var out map[string]any
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		rec, a, e := s.recordFor(r.Context(), tx, claims.ProducerID, eventID)
		if e != nil {
			return e
		}
		attestationID := ""
		provisional := true
		anchorStatus := ""
		if a != nil {
			attestationID = a.ID.String()
			provisional = false
			anchorStatus = a.AnchorStatus
		}
		out = map[string]any{
			"event": rec.Event, "sales": rec.Sales, "courtesy": rec.Courtesy,
			"attendance": rec.Attendance, "provisional": provisional, "attestation_id": attestationID,
			"anchor_status": anchorStatus,
		}
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// commitmentsReport é o relatório de contrapartida (declarado vs realizado).
func (s *Server) commitmentsReport(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var out map[string]any
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		rec, a, e := s.recordFor(r.Context(), tx, claims.ProducerID, eventID)
		if e != nil {
			return e
		}
		provisional := true
		attestationID := ""
		anchorStatus := ""
		if a != nil {
			provisional = false
			attestationID = a.ID.String()
			anchorStatus = a.AnchorStatus
		}
		rows := make([]map[string]any, 0, len(rec.Commitments))
		for _, c := range rec.Commitments {
			status := compareStatus(c)
			rows = append(rows, map[string]any{
				"kind": c.Kind, "category": c.Category, "target_type": c.TargetType,
				"target_value": c.TargetValue, "realized": c.Realized, "description": c.Description,
				"status": status,
			})
		}
		out = map[string]any{
			"half_price": rec.HalfPrice, "commitments": rows,
			"provisional": provisional, "attestation_id": attestationID, "anchor_status": anchorStatus,
		}
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// compareStatus compara o realizado com a meta (strings numéricas).
func compareStatus(c attest.RecordCommitment) string {
	if c.Realized == "" || c.TargetValue == "" {
		return "n/a"
	}
	real, err1 := strconv.ParseFloat(c.Realized, 64)
	target, err2 := strconv.ParseFloat(c.TargetValue, 64)
	if err1 != nil || err2 != nil {
		return "n/a"
	}
	if real >= target {
		return "cumprido"
	}
	return "nao_cumprido"
}

// ── verificação pública ──────────────────────────────────────────────────────

// publicAttestation devolve o atestado completo para verificação pública (sem auth).
func (s *Server) publicAttestation(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var producerID uuid.UUID
	if err := s.pool.QueryRow(r.Context(), `SELECT producer_id FROM public.attestation_index WHERE attestation_id=$1`, id).Scan(&producerID); err != nil {
		writeErr(w, http.StatusNotFound, "atestado não encontrado")
		return
	}
	var a *attest.Attestation
	var supersededBy *uuid.UUID
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		var e error
		a, e = attest.Get(r.Context(), tx, id)
		if e != nil {
			return e
		}
		if a != nil {
			supersededBy, e = attest.SupersededBy(r.Context(), tx, id)
		}
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if a == nil {
		writeErr(w, http.StatusNotFound, "atestado não encontrado")
		return
	}
	var rec attest.Record
	if err := json.Unmarshal(a.Payload, &rec); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ser, err := attest.SerializeRecord(rec)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// A chave pública vem do REGISTRO de chaves, resolvida pelo key_id do atestado — nunca
	// pela chave corrente (assim atestados antigos seguem verificáveis após rotação).
	var publicKey string
	if err := s.pool.QueryRow(r.Context(), `SELECT public_key FROM public.attestation_keys WHERE key_id=$1`, rec.KeyID).Scan(&publicKey); err != nil {
		writeErr(w, http.StatusInternalServerError, "chave de atestação não encontrada para o atestado")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":             a.ID,
		"event_id":       a.EventID,
		"version":        a.Version,
		"supersedes_id":  a.SupersedesID,
		"superseded_by":  supersededBy,
		"format_version": rec.FormatVersion,
		"key_id":         rec.KeyID,
		"public_key":     publicKey,
		"payload":        rec,
		"serialization":  string(ser),
		"digest":         hex.EncodeToString(a.Digest),
		"signature":      hex.EncodeToString(a.Signature),
		"anchor": map[string]any{
			"status":      a.AnchorStatus,
			"tx_hash":     a.AnchorTxHash,
			"anchored_at": a.AnchoredAt,
		},
	})
}

// nilStr converte string vazia em nil (colunas nullable).
func nilStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type halfPriceReq struct {
	// Mode: "quota" (cota própria) ou "linked" (a meia segue o estoque do lote pai).
	Mode string `json:"mode"`
	// TargetType/TargetValue só valem no modo cota: "percent" com 40 = 40% da capacidade.
	TargetType  string `json:"target_type"`
	TargetValue string `json:"target_value"`
}

// putHalfPrice grava a configuração de meia-entrada do evento.
//
// 40% é o DEFAULT do sistema, não uma trava: a Lei 12.933/2013 obriga o produtor, e recusar
// a configuração dele não o faz cumprir a lei — só o impede de operar. Abaixo disso a
// resposta traz o aviso que a tela mostra, e a escolha entra na trilha com valor, data e
// usuário. É o que existe para responder "quem decidiu isso" seis meses depois.
func (s *Server) putHalfPrice(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body halfPriceReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if body.Mode == "" {
		body.Mode = attest.ModeQuota
	}
	var out attest.HalfPriceAllowance
	err = s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		if out, e = attest.SetHalfPrice(r.Context(), tx, eventID, body.Mode, body.TargetType, body.TargetValue); e != nil {
			return e
		}
		return audit.Append(r.Context(), tx, audit.Event{
			Entity: audit.EntityEvent, EventID: &eventID,
			ActorKind: audit.ActorProducer, Actor: claims.Subject,
			ToStatus: "half_price_set",
			Details: map[string]any{
				"mode": out.Mode, "target_type": body.TargetType, "target_value": body.TargetValue,
				"quota": out.Quota, "legal_quota": out.LegalQuota, "below_legal": out.BelowLegal,
			},
		})
	})
	switch {
	case errors.Is(err, attest.ErrClosed):
		writeErr(w, http.StatusConflict, "evento já fechado")
	case err != nil:
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		resp := map[string]any{"half_price": out}
		if out.BelowLegal {
			resp["warning"] = "Esta cota está abaixo dos 40% que a Lei 12.933/2013 exige. " +
				"O cumprimento da lei é responsabilidade do produtor, e a escolha ficou registrada."
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// halfPriceHistory devolve a trilha da configuração de meia do evento: quem escolheu, quando
// e quanto. Sem ela, "quem decidiu vender 10% de meia" é pergunta sem resposta.
func (s *Server) halfPriceHistory(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var events []audit.Event
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		events, e = audit.EventHistory(r.Context(), tx, eventID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}
