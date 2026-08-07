// Package notify define o notificador (e-mail, WhatsApp, push) usado para entrega de
// ingresso e avisos. Interface trocável; a entrega real chega nas etapas de checkout/
// emissão/divulgação. Aqui só o seam, com um LogNotifier que apenas registra.
package notify

import (
	"context"
	"log/slog"
)

// Message é uma mensagem a entregar por algum canal.
type Message struct {
	Channel string // "email" | "whatsapp" | "push"
	To      string
	Subject string
	Body    string
}

// Notifier entrega mensagens.
type Notifier interface {
	Send(ctx context.Context, msg Message) error
}

// LogNotifier é o default: registra a mensagem em log em vez de entregar. Serve de
// no-op observável até haver provedores reais.
type LogNotifier struct{}

// NewLogNotifier constrói o notificador de log.
func NewLogNotifier() LogNotifier { return LogNotifier{} }

func (LogNotifier) Send(_ context.Context, msg Message) error {
	slog.Info("notify (stub)", "channel", msg.Channel, "to", msg.To, "subject", msg.Subject)
	return nil
}
