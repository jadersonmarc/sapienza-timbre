package notify

import (
	"encoding/base64"
	"fmt"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// RenderedMessage é o e-mail pronto para o provedor (texto + HTML + anexo opcional).
type RenderedMessage struct {
	To         string      `json:"to"`
	Subject    string      `json:"subject"`
	Text       string      `json:"text"`
	HTML       string      `json:"html"`
	Attachment *Attachment `json:"attachment,omitempty"`
}

// Attachment é um arquivo anexado (conteúdo em base64).
type Attachment struct {
	Filename    string `json:"filename"`
	Content     string `json:"content"`
	ContentType string `json:"content_type"`
}

// render monta subject/text/html (e anexo, quando houver QR) para a mensagem. O QR é
// anexo BEST-EFFORT: se a geração falhar, a mensagem sai mesmo assim com o link.
func render(m Message, publicBaseURL string) RenderedMessage {
	switch m.Kind {
	case KindAuthCode:
		return renderAuthCode(m)
	case KindRefunded:
		return renderRefund(m)
	case KindRefundRequested:
		return renderRefundRequested(m)
	case KindRefundRejected:
		return renderRefundRejected(m)
	case KindWaitlist:
		return RenderedMessage{To: m.To, Subject: m.Subject, Text: m.Body, HTML: "<p>" + htmlEscape(m.Body) + "</p>"}
	default:
		return renderTicket(m, publicBaseURL)
	}
}

func renderAuthCode(m Message) RenderedMessage {
	// O assunto contém o código de propósito: a pessoa lê na notificação do celular sem
	// abrir o e-mail — o que mais reduz abandono no checkout.
	subject := "Seu código: " + m.Code
	ignore := "Se não foi você quem pediu, pode ignorar este e-mail."
	text := fmt.Sprintf("Seu código de acesso é %s.\nEle vale por %d minutos.\n%s", m.Code, m.CodeMinutes, ignore)
	html := fmt.Sprintf(
		`<p>Seu código de acesso é:</p><p style="font-size:28px;font-weight:bold;letter-spacing:4px">%s</p>
		 <p>Ele vale por %d minutos.</p><p>%s</p>`,
		htmlEscape(m.Code), m.CodeMinutes, ignore)
	return RenderedMessage{To: m.To, Subject: subject, Text: text, HTML: html}
}

// renderRefundRequested confirma que o pedido chegou e diz até quando a casa responde. Um
// pedido que some sem aviso vira ligação, e a ligação vira contestação.
func renderRefundRequested(m Message) RenderedMessage {
	subject := "Recebemos seu pedido de devolução"
	prazo := ""
	if m.RespondsBy != "" {
		prazo = fmt.Sprintf(" O produtor tem até %s para responder.", m.RespondsBy)
	}
	text := fmt.Sprintf("Recebemos seu pedido de devolução do ingresso para %s.%s\nVocê será avisado assim que houver uma resposta.", m.EventName, prazo)
	html := fmt.Sprintf(
		`<p>Recebemos seu pedido de devolução do ingresso para <strong>%s</strong>.%s</p>
		 <p>Você será avisado assim que houver uma resposta.</p>`,
		htmlEscape(m.EventName), htmlEscape(prazo))
	return RenderedMessage{To: m.To, Subject: subject, Text: text, HTML: html}
}

// renderRefundRejected entrega a recusa COM o motivo. Recusa sem explicação é a que volta.
func renderRefundRejected(m Message) RenderedMessage {
	subject := "Sobre seu pedido de devolução"
	motivo := m.DecisionReason
	if motivo == "" {
		motivo = "sem motivo informado"
	}
	text := fmt.Sprintf("Seu pedido de devolução do ingresso para %s não foi aceito.\nMotivo: %s\n\nSeu ingresso continua válido.", m.EventName, motivo)
	html := fmt.Sprintf(
		`<p>Seu pedido de devolução do ingresso para <strong>%s</strong> não foi aceito.</p>
		 <p><strong>Motivo:</strong> %s</p><p>Seu ingresso continua válido.</p>`,
		htmlEscape(m.EventName), htmlEscape(motivo))
	return RenderedMessage{To: m.To, Subject: subject, Text: text, HTML: html}
}

func renderTicket(m Message, publicBaseURL string) RenderedMessage {
	meURL := m.MeTicketsURL
	if meURL == "" {
		meURL = strings.TrimRight(publicBaseURL, "/") + "/ingressos"
	}
	subject := m.EventName
	if subject == "" {
		subject = "Seu ingresso Timbre"
	}
	var b strings.Builder
	b.WriteString(m.EventName)
	b.WriteString("\nData: ")
	b.WriteString(m.EventStarts)
	if m.VenueCity != "" || m.Address != "" {
		b.WriteString("\nLocal: ")
		b.WriteString(strings.TrimSpace(strings.Join([]string{m.Address, m.VenueCity}, " — ")))
	}
	if m.SectorName != "" {
		b.WriteString("\nSetor: ")
		b.WriteString(m.SectorName)
	}
	if m.SeatLabel != "" {
		b.WriteString("\nAssento: ")
		b.WriteString(m.SeatLabel)
	}
	b.WriteString("\n\nSeus ingressos: ")
	b.WriteString(meURL)

	html := fmt.Sprintf(
		`<p><strong>%s</strong></p><p>Data: %s</p>%s%s<p>Seus ingressos sempre atualizados: <a href="%s">meus ingressos</a>.</p>`,
		htmlEscape(m.EventName), htmlEscape(m.EventStarts),
		lineHTML("Local", strings.TrimSpace(strings.Join([]string{m.Address, m.VenueCity}, " — "))),
		lineHTML("Setor", m.SectorName)+lineHTML("Assento", m.SeatLabel),
		htmlEscape(meURL))

	rm := RenderedMessage{To: m.To, Subject: subject, Text: b.String(), HTML: html}
	if m.QRContent != "" {
		if png, err := qrcode.Encode(m.QRContent, qrcode.Medium, 320); err == nil {
			rm.Attachment = &Attachment{
				Filename: "ingresso.png", Content: base64.StdEncoding.EncodeToString(png), ContentType: "image/png",
			}
		}
		// Falha na geração do QR: envia mesmo assim com o link (nunca deixa de enviar).
	}
	return rm
}

func renderRefund(m Message) RenderedMessage {
	subject := "Estorno do seu ingresso"
	value := fmt.Sprintf("%d", m.OrderValueCents)
	if m.OrderValueCents > 0 {
		value = fmt.Sprintf("%.2f", float64(m.OrderValueCents)/100)
	}
	text := fmt.Sprintf("Confirmamos o estorno do seu ingresso para %s, no valor de R$ %s.", m.EventName, value)
	html := fmt.Sprintf(`<p>Confirmamos o estorno do seu ingresso para <strong>%s</strong>, no valor de <strong>R$ %s</strong>.</p>`, htmlEscape(m.EventName), value)
	return RenderedMessage{To: m.To, Subject: subject, Text: text, HTML: html}
}

func lineHTML(label, value string) string {
	if value == "" {
		return ""
	}
	return fmt.Sprintf("<p>%s: %s</p>", label, htmlEscape(value))
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
