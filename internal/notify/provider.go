package notify

import (
	"context"
	"errors"
	"log/slog"
)

// Provider envia o e-mail pronto e devolve o id da mensagem no provedor.
type Provider interface {
	Send(ctx context.Context, m RenderedMessage) (providerID string, err error)
}

// sendError classifica o erro para a política de retry: retryable (5xx, rate limit) ou
// permanente (4xx). Nunca contém segredo.
type sendError struct {
	retryable bool
	msg       string
}

func (e *sendError) Error() string { return e.msg }

// retryableError marca um erro que merece nova tentativa com backoff.
func retryableError(msg string) error { return &sendError{retryable: true, msg: msg} }

// permanentError marca um erro que não adianta repetir (4xx).
func permanentError(msg string) error { return &sendError{retryable: false, msg: msg} }

// isRetryable diz se o erro justifica nova tentativa.
func isRetryable(err error) bool {
	var se *sendError
	return errors.As(err, &se) && se.retryable
}

// LogProvider é o default: registra a mensagem em log e marca como enviada. Sem provedor
// real, é o que mantém o comportamento observável atual (TIMBRE_NOTIFIER=log).
type LogProvider struct{}

func (LogProvider) Send(_ context.Context, m RenderedMessage) (string, error) {
	slog.Info("notify", "to", m.To, "subject", m.Subject)
	return "log", nil
}
