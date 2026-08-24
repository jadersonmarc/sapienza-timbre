package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/market"
	"github.com/jadersonmarc/sapienza-timbre/internal/nft"
	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
	"github.com/jadersonmarc/sapienza-timbre/internal/transfer"
)

// Onda 2 — posse do comprador em CUSTÓDIA DE PLATAFORMA. A posse é o vínculo em
// public.ticket_directory (subject_id); cada subject tem uma carteira de plataforma
// (public.wallets, custody 'platform') para o motor de transferência/royalty existente
// reatribuir o dono. O que depende de rede (carteira externa/on-chain) segue stub.

// ownsTicket confirma que o subject é dono do ingresso ativo e devolve o produtor dele.
func (s *Server) ownsTicket(ctx context.Context, subjectID, ticketID uuid.UUID) (uuid.UUID, error) {
	var producerID uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT producer_id FROM ticket_directory
		 WHERE ticket_id=$1 AND subject_id=$2 AND status='active'`, ticketID, subjectID).Scan(&producerID)
	return producerID, err
}

// resolveWalletTx acha (ou cria) a carteira de plataforma do subject, dentro da tx.
func resolveWalletTx(ctx context.Context, tx pgx.Tx, subjectID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM wallets WHERE subject_id=$1 AND custody='platform' LIMIT 1`, subjectID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO wallets (subject_id, address, chain, custody)
		VALUES ($1::uuid, 'plat:' || ($1::uuid)::text, 'base', 'platform') RETURNING id`, subjectID).Scan(&id)
	return id, err
}

// resolveSubjectByEmailTx acha/cria o subject por e-mail, dentro da tx.
func resolveSubjectByEmailTx(ctx context.Context, tx pgx.Tx, email string) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM subjects WHERE lower(email)=lower($1) ORDER BY created_at LIMIT 1`, email).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO subjects (email) VALUES ($1)
		 ON CONFLICT (lower(email)) WHERE email IS NOT NULL
		 DO UPDATE SET updated_at = now()
		 RETURNING id`, email).Scan(&id)
	return id, err
}

type buyerTransferReq struct {
	ToEmail string `json:"to_email"`
}

// buyerTransfer transfere a titularidade do ingresso para outro e-mail (presente de
// cortesia — preço 0). Respeita janela de contestação, teto e disputa (transfer.Execute).
// Reatribui o dono em ticket_directory (o remetente deixa de ver; o destinatário passa a
// ver, e vincula quando criar/entrar na conta com esse e-mail).
func (s *Server) buyerTransfer(w http.ResponseWriter, r *http.Request, subjectID uuid.UUID) {
	ticketID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body buyerTransferReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	toEmail := normalizeEmail(body.ToEmail)
	if !looksLikeEmail(toEmail) {
		writeErr(w, http.StatusBadRequest, "informe um e-mail de destino válido")
		return
	}
	producerID, err := s.ownsTicket(r.Context(), subjectID, ticketID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusForbidden, "este ingresso não é seu ou não pode ser transferido")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	err = s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		toSubject, e := resolveSubjectByEmailTx(r.Context(), tx, toEmail)
		if e != nil {
			return e
		}
		toWallet, e := resolveWalletTx(r.Context(), tx, toSubject)
		if e != nil {
			return e
		}
		if _, e := transfer.Execute(r.Context(), tx, ticketID, toWallet, 0); e != nil {
			return e
		}
		_, e = tx.Exec(r.Context(), `
			UPDATE ticket_directory SET subject_id=$2, buyer_email=$3 WHERE ticket_id=$1`,
			ticketID, toSubject, toEmail)
		return e
	})
	if err != nil {
		writeTransferErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type buyerListingReq struct {
	PriceCents int64 `json:"price_cents"`
}

// buyerCreateListing anuncia o ingresso do comprador no mercado secundário (teto/janela
// validados). A compra do anúncio já é pública.
func (s *Server) buyerCreateListing(w http.ResponseWriter, r *http.Request, subjectID uuid.UUID) {
	ticketID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body buyerListingReq
	if err := decode(w, r, &body); err != nil || body.PriceCents <= 0 {
		writeErr(w, http.StatusBadRequest, "price_cents obrigatório (> 0)")
		return
	}
	producerID, err := s.ownsTicket(r.Context(), subjectID, ticketID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusForbidden, "este ingresso não é seu ou não pode ser anunciado")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var listing market.Listing
	err = s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		var e error
		listing, e = market.CreateListing(r.Context(), tx, producerID, ticketID, body.PriceCents)
		return e
	})
	if err != nil {
		writeTransferErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, listing)
}

// buyerReissue reemite o ingresso do comprador (§3.2): queima o anterior e gera um QR
// novo. Uso operacional urgente — trocou de celular / perdeu o e-mail antigo e recuperou a
// conta. Atualiza o ticket_directory para o novo ingresso/token.
func (s *Server) buyerReissue(w http.ResponseWriter, r *http.Request, subjectID uuid.UUID) {
	ticketID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	producerID, err := s.ownsTicket(r.Context(), subjectID, ticketID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusForbidden, "este ingresso não é seu")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var newID uuid.UUID
	err = s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		var e error
		newID, e = nft.Reissue(r.Context(), tx, s.signer, producerID, ticketID)
		if e != nil {
			return e
		}
		token, e := ticketing.TicketToken(r.Context(), tx, newID)
		if e != nil {
			return e
		}
		_, e = tx.Exec(r.Context(), `
			UPDATE ticket_directory SET ticket_id=$2, token=$3
			 WHERE producer_id=$1 AND ticket_id=$4`, producerID, newID, token, ticketID)
		return e
	})
	if err != nil {
		if errors.Is(err, nft.ErrNotReissuable) {
			writeErr(w, http.StatusConflict, "ingresso não pode ser reemitido")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ticket_id": newID})
}

// writeTransferErr mapeia os erros de transferência/mercado para códigos claros.
func writeTransferErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, transfer.ErrPriceCap):
		writeErr(w, http.StatusBadRequest, "preço acima do teto de revenda")
	case errors.Is(err, transfer.ErrNotActive):
		writeErr(w, http.StatusConflict, "ingresso não está ativo")
	case errors.Is(err, transfer.ErrNotTransferable):
		writeErr(w, http.StatusConflict, "ingresso ainda não é transferível (janela de contestação)")
	case errors.Is(err, transfer.ErrDisputed):
		writeErr(w, http.StatusConflict, "ingresso em disputa")
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}
