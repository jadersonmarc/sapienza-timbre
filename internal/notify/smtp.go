package notify

import (
	"context"
	"fmt"
	"net"
	"net/smtp"
	"strings"
)

// SMTPConfig é a configuração do provedor transacional. Provider-agnóstico (SMTP fala com
// Resend/SES/Postmark/etc.) — sem embutir vendedor. Vazio = não configurado.
type SMTPConfig struct {
	Host     string
	Port     string
	Username string
	Password string
	From     string // "Timbre <no-reply@dominio>"
}

// Configured diz se há SMTP para ligar (todos os campos essenciais presentes).
func (c SMTPConfig) Configured() bool {
	return c.Host != "" && c.Port != "" && c.From != ""
}

// SMTPNotifier entrega e-mail de verdade por SMTP (STARTTLS na porta de submissão, ex.
// 587). Implementa notify.Notifier — o mesmo seam do QR e dos avisos. Só o canal "email"
// é entregue; os demais canais caem no log até haver provedor próprio.
type SMTPNotifier struct {
	cfg      SMTPConfig
	fallback Notifier // para canais não-email (whatsapp/push) — LogNotifier
}

// NewSMTPNotifier constrói o notificador SMTP com fallback de log para canais não-email.
func NewSMTPNotifier(cfg SMTPConfig) SMTPNotifier {
	return SMTPNotifier{cfg: cfg, fallback: LogNotifier{}}
}

func (n SMTPNotifier) Send(ctx context.Context, msg Message) error {
	if msg.Channel != "email" {
		return n.fallback.Send(ctx, msg)
	}
	addr := net.JoinHostPort(n.cfg.Host, n.cfg.Port)
	// Cabeçalhos mínimos + corpo texto. O QR é um token; o OTP é um código — texto basta.
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", n.cfg.From)
	fmt.Fprintf(&b, "To: %s\r\n", msg.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", msg.Subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
	b.WriteString(msg.Body)

	var auth smtp.Auth
	if n.cfg.Username != "" {
		auth = smtp.PlainAuth("", n.cfg.Username, n.cfg.Password, n.cfg.Host)
	}
	// smtp.SendMail negocia STARTTLS quando o servidor anuncia (porta 587 de submissão).
	if err := smtp.SendMail(addr, auth, senderAddress(n.cfg.From), []string{msg.To}, []byte(b.String())); err != nil {
		return fmt.Errorf("enviar e-mail (smtp): %w", err)
	}
	return nil
}

// senderAddress extrai o endereço de "Nome <addr>" (o envelope MAIL FROM não leva o nome).
func senderAddress(from string) string {
	if i := strings.LastIndex(from, "<"); i >= 0 {
		if j := strings.Index(from[i:], ">"); j >= 0 {
			return from[i+1 : i+j]
		}
	}
	return from
}
