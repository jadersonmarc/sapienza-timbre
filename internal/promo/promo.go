// Package promo é divulgação e engajamento (Etapa 2.8): campanhas com UTM, pixels do
// evento, lista de espera com aviso na virada de lote, e o perfil do público. Roda sob
// tenancy.WithTenant. Os disparos usam o Notifier (trocável).
package promo

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/notify"
)

// Campaign é uma campanha com UTM.
type Campaign struct {
	ID          uuid.UUID `json:"id"`
	EventID     uuid.UUID `json:"event_id"`
	Name        string    `json:"name"`
	UTMSource   *string   `json:"utm_source,omitempty"`
	UTMMedium   *string   `json:"utm_medium,omitempty"`
	UTMCampaign *string   `json:"utm_campaign,omitempty"`
	Clicks      int       `json:"clicks"`
}

// CreateCampaign cria uma campanha e registra o índice público (para o clique).
func CreateCampaign(ctx context.Context, tx pgx.Tx, producerID, eventID uuid.UUID, name, source, medium, campaign string) (Campaign, error) {
	var c Campaign
	err := tx.QueryRow(ctx, `
		INSERT INTO campaigns (event_id, name, utm_source, utm_medium, utm_campaign)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id, event_id, name, utm_source, utm_medium, utm_campaign, clicks`,
		eventID, name, nilStr(source), nilStr(medium), nilStr(campaign)).
		Scan(&c.ID, &c.EventID, &c.Name, &c.UTMSource, &c.UTMMedium, &c.UTMCampaign, &c.Clicks)
	if err != nil {
		return Campaign{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO public.campaign_index (campaign_id, producer_id, event_id) VALUES ($1,$2,$3)`, c.ID, producerID, eventID); err != nil {
		return Campaign{}, err
	}
	return c, nil
}

// ListCampaigns lista as campanhas de um evento.
func ListCampaigns(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) ([]Campaign, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, event_id, name, utm_source, utm_medium, utm_campaign, clicks
		  FROM campaigns WHERE event_id=$1 ORDER BY created_at`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Campaign
	for rows.Next() {
		var c Campaign
		if err := rows.Scan(&c.ID, &c.EventID, &c.Name, &c.UTMSource, &c.UTMMedium, &c.UTMCampaign, &c.Clicks); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// TrackClick incrementa o clique de uma campanha (link parametrizado público).
func TrackClick(ctx context.Context, tx pgx.Tx, campaignID uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE campaigns SET clicks = clicks + 1 WHERE id=$1`, campaignID)
	return err
}

// JoinWaitlist inscreve um e-mail na lista de espera do evento (idempotente).
func JoinWaitlist(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, email string) error {
	_, err := tx.Exec(ctx, `INSERT INTO waitlist (event_id, email) VALUES ($1,$2) ON CONFLICT (event_id, email) DO NOTHING`, eventID, email)
	return err
}

// NotifyWaitlist avisa quem ainda não foi avisado (ex.: virada de lote / vagas). Marca
// notified_at e dispara pelo Notifier. Idempotente. Devolve quantos foram avisados.
func NotifyWaitlist(ctx context.Context, tx pgx.Tx, notifier notify.Notifier, eventID uuid.UUID, reason string) (int, error) {
	rows, err := tx.Query(ctx, `SELECT id, email FROM waitlist WHERE event_id=$1 AND notified_at IS NULL`, eventID)
	if err != nil {
		return 0, err
	}
	type w struct {
		id    uuid.UUID
		email string
	}
	var list []w
	for rows.Next() {
		var it w
		if err := rows.Scan(&it.id, &it.email); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, it := range list {
		if _, err := tx.Exec(ctx, `UPDATE waitlist SET notified_at=now() WHERE id=$1`, it.id); err != nil {
			return 0, err
		}
		if notifier != nil {
			_ = notifier.Send(ctx, notify.Message{Channel: "email", To: it.email, Subject: "Timbre — vaga disponível", Body: reason})
		}
	}
	return len(list), nil
}

// SourceStat é o perfil do público por fonte de aquisição.
type SourceStat struct {
	Source  string `json:"source"`
	Orders  int    `json:"orders"`
	Tickets int    `json:"tickets"`
}

// AudienceProfile agrega as compras pagas por fonte (UTM ou 'direto').
func AudienceProfile(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) ([]SourceStat, error) {
	rows, err := tx.Query(ctx, `
		SELECT COALESCE(c.utm_source, 'direto') AS src,
		       count(DISTINCT o.id) AS orders,
		       count(t.id) AS tickets
		  FROM orders o
		  LEFT JOIN campaigns c ON c.id = o.campaign_id
		  LEFT JOIN tickets t ON t.order_id = o.id
		 WHERE o.event_id = $1 AND o.status = 'paid'
		 GROUP BY 1 ORDER BY orders DESC`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceStat
	for rows.Next() {
		var s SourceStat
		if err := rows.Scan(&s.Source, &s.Orders, &s.Tickets); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Pixels são os pixels de rastreamento do evento.
type Pixels struct {
	MetaPixel string `json:"meta_pixel,omitempty"`
	GoogleID  string `json:"google_id,omitempty"`
}

// EventPixels devolve os pixels do evento (para a página pública de checkout).
func EventPixels(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (Pixels, error) {
	var p Pixels
	var meta, google *string
	if err := tx.QueryRow(ctx, `SELECT meta_pixel, google_id FROM events WHERE id=$1`, eventID).Scan(&meta, &google); err != nil {
		return p, err
	}
	if meta != nil {
		p.MetaPixel = *meta
	}
	if google != nil {
		p.GoogleID = *google
	}
	return p, nil
}

func nilStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
