// Package notify define o notificador (e-mail, WhatsApp, push) usado para entrega de
// código de acesso, ingresso e avisos. O envio é ASSÍNCRONO: Send apenas enfileira um
// registro em public.notifications e retorna — nunca bloqueia o caminho que o disparou.
// Um worker em segundo plano drena a fila e envia pelo provedor configurado.
package notify

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Kinds de mensagem.
const (
	KindAuthCode = "auth_code"
	KindTicket   = "ticket_issued"
	KindRefunded = "order_refunded"
	KindWaitlist = "waitlist"
)

// Message é uma mensagem estruturada a entregar por algum canal. O worker renderiza o
// e-mail final (texto + HTML + anexo) a partir destes campos.
type Message struct {
	Channel string
	To      string
	Kind    string // auth_code | ticket_issued | order_refunded | waitlist
	// Referências soltas para o registro e o painel.
	ProducerID *uuid.UUID
	EventID    *uuid.UUID
	SubjectID  *uuid.UUID
	TicketID   *uuid.UUID
	OrderID    *uuid.UUID
	// Dados da mensagem.
	Code            string
	CodeMinutes     int
	EventName       string
	EventStarts     string
	VenueCity       string
	Address         string
	SectorName      string
	SeatLabel       string
	OrderValueCents int64
	QRContent       string // conteúdo do QR (anexo de imagem no ingresso)
	MeTicketsURL    string // link para "meus ingressos" (source of truth)
	// Mensagem livre (kind waitlist): assunto e corpo simples.
	Subject string
	Body    string
}

// Notifier entrega mensagens. A implementação real enfileira em public.notifications.
type Notifier interface {
	Send(ctx context.Context, msg Message) error
}

// Service implementa Notifier enfileirando a notificação e retornando imediatamente.
// O envio em si é do worker. publicBaseURL é usado para montar links das mensagens.
type Service struct {
	pool          *pgxpool.Pool
	publicBaseURL string
}

// NewService constrói o serviço de notificação assíncrono.
func NewService(pool *pgxpool.Pool, publicBaseURL string) *Service {
	return &Service{pool: pool, publicBaseURL: publicBaseURL}
}

// Send renderiza e enfileira a mensagem (status 'queued'). Retorna sem esperar o envio.
func (s *Service) Send(ctx context.Context, msg Message) error {
	if msg.To == "" || msg.Kind == "" {
		return nil // sem destino/canário: nada a enviar
	}
	if msg.Channel != "" && msg.Channel != "email" {
		slog.Info("notify canal não-email ignorado", "channel", msg.Channel)
		return nil
	}
	rendered := render(msg, s.publicBaseURL)
	payload, err := json.Marshal(rendered)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO public.notifications
		    (producer_id, event_id, kind, to_email, subject_id, ticket_id, order_id, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		msg.ProducerID, msg.EventID, msg.Kind, msg.To, msg.SubjectID, msg.TicketID, msg.OrderID, payload)
	return err
}
