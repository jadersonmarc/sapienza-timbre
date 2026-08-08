// Package nft é a gestão do ingresso como NFT (Etapa 1.9): metadados públicos ERC-1155
// (sem dado pessoal), estado do token, exportação para carteira externa, disputa (bloqueia
// transferência, não a entrada) e reemissão. Roda sob tenancy.WithTenant. Guardrails: a
// validade do QR não depende da rede; exportar não quebra a portaria; metadados públicos
// nunca carregam dado pessoal.
package nft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
)

var (
	// ErrAlreadyDisputed: já existe disputa aberta para o ingresso.
	ErrAlreadyDisputed = errors.New("nft: ingresso já em disputa")
	// ErrNotReissuable: ingresso não pode ser reemitido (não está ativo).
	ErrNotReissuable = errors.New("nft: ingresso não reemitível")
)

type attr struct {
	Trait string `json:"trait_type"`
	Value any    `json:"value"`
}

// GenerateMetadata (re)gera os metadados públicos ERC-1155 do token a partir do evento/
// lote/assento — SEM DADO PESSOAL. Chamado na emissão e na reemissão.
func GenerateMetadata(ctx context.Context, tx pgx.Tx, producerID, ticketID uuid.UUID) error {
	var eventTitle, lotName string
	var startsAt *time.Time
	var address, sector, row, number *string
	var face int64
	err := tx.QueryRow(ctx, `
		SELECT e.title, COALESCE(l.name,''), e.starts_at, e.address, se.name, s.row_label, s.number, l.price_cents
		  FROM tickets t
		  JOIN events e ON e.id = t.event_id
		  JOIN lots l ON l.id = t.lot_id
		  LEFT JOIN seats s ON s.id = t.seat_id
		  LEFT JOIN sectors se ON se.id = s.sector_id
		 WHERE t.id = $1`, ticketID).Scan(&eventTitle, &lotName, &startsAt, &address, &sector, &row, &number, &face)
	if err != nil {
		return err
	}
	attrs := []attr{{Trait: "Evento", Value: eventTitle}, {Trait: "Lote", Value: lotName}, {Trait: "Valor de face (centavos)", Value: face}}
	if startsAt != nil {
		attrs = append(attrs, attr{Trait: "Data", Value: startsAt.Format(time.RFC3339)})
	}
	if address != nil {
		attrs = append(attrs, attr{Trait: "Local", Value: *address})
	}
	if sector != nil {
		attrs = append(attrs, attr{Trait: "Setor", Value: *sector})
	}
	if row != nil {
		attrs = append(attrs, attr{Trait: "Fileira", Value: *row})
	}
	if number != nil {
		attrs = append(attrs, attr{Trait: "Assento", Value: *number})
	}
	name := eventTitle
	if sector != nil && row != nil && number != nil {
		name = fmt.Sprintf("%s — %s %s%s", eventTitle, *sector, *row, *number)
	}
	attrsJSON, _ := json.Marshal(attrs)
	_, err = tx.Exec(ctx, `
		INSERT INTO public.token_metadata (ticket_id, producer_id, name, description, attributes, updated_at)
		VALUES ($1,$2,$3,$4,$5::jsonb, now())
		ON CONFLICT (ticket_id) DO UPDATE
		   SET producer_id=EXCLUDED.producer_id, name=EXCLUDED.name, description=EXCLUDED.description,
		       attributes=EXCLUDED.attributes, updated_at=now()`,
		ticketID, producerID, name, "Ingresso Timbre", string(attrsJSON))
	return err
}

// State é o estado do token exposto ao portador.
type State struct {
	Lifecycle         string     `json:"lifecycle"` // intransferivel | transferivel | utilizado | queimado | transferido | cancelado
	Chain             string     `json:"chain"`     // aguardando_emissao | emitido | falha
	Custody           string     `json:"custody"`   // platform | external
	TransferableAfter *time.Time `json:"transferable_after,omitempty"`
	Disputed          bool       `json:"disputed"`
}

// TicketState computa o estado atual do token (calculado, não armazenado).
func TicketState(ctx context.Context, tx pgx.Tx, ticketID uuid.UUID) (State, error) {
	var st State
	var status, chainStatus, custody string
	var ta time.Time
	if err := tx.QueryRow(ctx, `SELECT status, chain_status, custody, transferable_after FROM tickets WHERE id=$1`, ticketID).
		Scan(&status, &chainStatus, &custody, &ta); err != nil {
		return State{}, err
	}
	st.Custody = custody
	st.TransferableAfter = &ta
	var used bool
	_ = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM checkins WHERE ticket_id=$1 AND NOT is_reentry)`, ticketID).Scan(&used)
	st.Disputed, _ = HasOpenDispute(ctx, tx, ticketID)

	switch {
	case status == "burned":
		st.Lifecycle = "queimado"
	case status == "transferred":
		st.Lifecycle = "transferido"
	case status == "cancelled":
		st.Lifecycle = "cancelado"
	case used:
		st.Lifecycle = "utilizado"
	case time.Now().Before(ta):
		st.Lifecycle = "intransferivel"
	default:
		st.Lifecycle = "transferivel"
	}
	switch chainStatus {
	case "minted":
		st.Chain = "emitido"
	case "failed":
		st.Chain = "falha"
	default:
		st.Chain = "aguardando_emissao"
	}
	return st, nil
}

// ExportTicket passa a custódia ao participante (carteira externa). Não toca status/
// assinatura — o QR segue válido e a entrada na portaria continua funcionando.
func ExportTicket(ctx context.Context, tx pgx.Tx, ticketID uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE tickets SET custody='external', exported_at=now(), updated_at=now() WHERE id=$1`, ticketID)
	return err
}

// OpenDispute abre uma disputa (bloqueia transferência; não bloqueia entrada).
func OpenDispute(ctx context.Context, tx pgx.Tx, ticketID uuid.UUID, reason string) error {
	_, err := tx.Exec(ctx, `INSERT INTO ticket_disputes (ticket_id, reason) VALUES ($1,$2)`, ticketID, nilStr(reason))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrAlreadyDisputed
	}
	return err
}

// HasOpenDispute diz se o ingresso tem disputa aberta.
func HasOpenDispute(ctx context.Context, tx pgx.Tx, ticketID uuid.UUID) (bool, error) {
	var open bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_disputes WHERE ticket_id=$1 AND status='open')`, ticketID).Scan(&open)
	return open, err
}

// Reissue queima o ingresso atual e emite um novo (assinado), preservando evento/lote/
// assento/dono — para casos de perda de acesso. O antigo queimado é a trilha de auditoria.
func Reissue(ctx context.Context, tx pgx.Tx, signer *ticketing.Signer, producerID, ticketID uuid.UUID) (uuid.UUID, error) {
	var eventID, lotID uuid.UUID
	var orderID, seatID, ownerSubject, ownerWallet *uuid.UUID
	var transferableAfter time.Time
	var halfPrice bool
	var status string
	err := tx.QueryRow(ctx, `
		SELECT event_id, lot_id, order_id, seat_id, owner_subject_id, owner_wallet_id, transferable_after, half_price, status
		  FROM tickets WHERE id=$1`, ticketID).
		Scan(&eventID, &lotID, &orderID, &seatID, &ownerSubject, &ownerWallet, &transferableAfter, &halfPrice, &status)
	if err != nil {
		return uuid.Nil, err
	}
	if status != "active" {
		return uuid.Nil, ErrNotReissuable
	}
	if _, err := tx.Exec(ctx, `UPDATE tickets SET status='burned', updated_at=now() WHERE id=$1`, ticketID); err != nil {
		return uuid.Nil, err
	}
	var newID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO tickets (event_id, lot_id, order_id, seat_id, owner_subject_id, owner_wallet_id, transferable_after, half_price, status, chain_status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'active','none') RETURNING id`,
		eventID, lotID, orderID, seatID, ownerSubject, ownerWallet, transferableAfter, halfPrice).Scan(&newID); err != nil {
		return uuid.Nil, err
	}
	// A ocupação do assento passa ao novo ingresso.
	if seatID != nil {
		if _, err := tx.Exec(ctx, `UPDATE seat_occupancy SET ticket_id=$2 WHERE ticket_id=$1 AND kind='ticket'`, ticketID, newID); err != nil {
			return uuid.Nil, err
		}
	}
	if err := ticketing.SignTicket(ctx, tx, signer, newID); err != nil {
		return uuid.Nil, err
	}
	if err := GenerateMetadata(ctx, tx, producerID, newID); err != nil {
		return uuid.Nil, err
	}
	return newID, nil
}

func nilStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
