// Package audit é a trilha append-only das ações do produtor: quem fez, quando, no quê e por
// quê. Uma tabela só, por tenant — duas significariam dois lugares para procurar quando
// alguém contesta, e duas chances de esquecer de gravar.
//
// Nada aqui atualiza: decisão tomada não se edita, e uma decisão que muda vira outra linha.
package audit

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Entidades sobre as quais a trilha fala.
const (
	EntityRefundRequest = "refund_request"
	EntityTicket        = "ticket"
	EntityCourtesy      = "courtesy"
	EntityExport        = "export"
)

// Quem agiu.
const (
	ActorBuyer    = "buyer"
	ActorProducer = "producer"
	ActorAdmin    = "admin"
	ActorSystem   = "system"
)

// Event é uma linha da trilha.
type Event struct {
	Entity     string         `json:"entity"`
	RequestID  *uuid.UUID     `json:"request_id,omitempty"`
	TicketID   *uuid.UUID     `json:"ticket_id,omitempty"`
	ActorKind  string         `json:"actor_kind"`
	Actor      string         `json:"actor,omitempty"`
	FromStatus string         `json:"from_status,omitempty"`
	ToStatus   string         `json:"to_status"`
	Reason     string         `json:"reason,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
	At         time.Time      `json:"at"`
}

// Append grava uma linha na transação de quem chama — a trilha morre junto com a ação que
// não aconteceu, em vez de registrar algo que foi desfeito.
func Append(ctx context.Context, tx pgx.Tx, e Event) error {
	details := []byte(`{}`)
	if len(e.Details) > 0 {
		if b, err := json.Marshal(e.Details); err == nil {
			details = b
		}
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO audit_events (entity, request_id, ticket_id, actor_kind, actor,
		                          from_status, to_status, reason, details)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		e.Entity, e.RequestID, e.TicketID, e.ActorKind, nilIfEmpty(e.Actor),
		nilIfEmpty(e.FromStatus), e.ToStatus, nilIfEmpty(e.Reason), details)
	return err
}

// TicketHistory devolve a trilha de um ingresso, em ordem. É o que o produtor mostra quando
// o comprador contesta.
func TicketHistory(ctx context.Context, tx pgx.Tx, ticketID uuid.UUID) ([]Event, error) {
	rows, err := tx.Query(ctx, `
		SELECT entity, actor_kind, COALESCE(actor,''), COALESCE(from_status,''), to_status,
		       COALESCE(reason,''), details, at
		  FROM audit_events WHERE ticket_id=$1 ORDER BY at, id`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var raw []byte
		if err := rows.Scan(&e.Entity, &e.ActorKind, &e.Actor, &e.FromStatus, &e.ToStatus,
			&e.Reason, &raw, &e.At); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &e.Details)
		e.TicketID = &ticketID
		out = append(out, e)
	}
	return out, rows.Err()
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
