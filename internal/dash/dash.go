// Package dash agrega os números dos painéis (Etapa 1.7). As funções de produtor
// recebem uma pgx.Tx já escopada por tenancy.WithTenant; as de plataforma varrem os
// schemas de produtor pelo pool. Leitura só (SELECT).
package dash

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"
)

// LotSales é a linha da curva de venda por lote (modelo de contadores derivado).
type LotSales struct {
	LotID        uuid.UUID `json:"lot_id"`
	Name         string    `json:"name"`
	PriceCents   int64     `json:"price_cents"`
	Quantity     int       `json:"quantity"`
	SoldCount    int       `json:"sold_count"`
	HeldCount    int       `json:"held_count"`
	RevenueCents int64     `json:"revenue_cents"`
}

// SalesByLot devolve, por lote, o vendido e a receita (de ordens pagas). O lote não tem
// mais 'status': a vigência é derivada (ver catalog.CurrentLot).
func SalesByLot(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) ([]LotSales, error) {
	rows, err := tx.Query(ctx, `
		SELECT l.id, l.name, l.price_cents, l.quantity, l.sold_count, l.held_count,
		       COALESCE((
		         SELECT SUM(oi.unit_price_cents * oi.quantity)
		           FROM order_items oi JOIN orders o ON o.id = oi.order_id
		          WHERE oi.lot_id = l.id AND o.status = 'paid'), 0)
		  FROM lots l
		 WHERE l.event_id = $1
		 ORDER BY l.sort_order, l.created_at`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LotSales
	for rows.Next() {
		var s LotSales
		if err := rows.Scan(&s.LotID, &s.Name, &s.PriceCents, &s.Quantity, &s.SoldCount, &s.HeldCount, &s.RevenueCents); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Occupancy é a ocupação do mapa (assentos vendidos vs total).
type Occupancy struct {
	SeatsTotal int `json:"seats_total"`
	SeatsSold  int `json:"seats_sold"`
}

// EventOccupancy conta assentos do evento e quantos têm ingresso vivo.
func EventOccupancy(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (Occupancy, error) {
	var o Occupancy
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM seats s JOIN sectors se ON se.id = s.sector_id WHERE se.event_id = $1`, eventID).Scan(&o.SeatsTotal); err != nil {
		return o, err
	}
	err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM tickets
		 WHERE event_id = $1 AND seat_id IS NOT NULL AND status IN ('active','used')`, eventID).Scan(&o.SeatsSold)
	return o, err
}

// Checkin é o andamento da portaria.
type Checkin struct {
	TicketsTotal int `json:"tickets_total"`
	Admitted     int `json:"admitted"`
}

// CheckinProgress conta ingressos e admissões primárias.
func CheckinProgress(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (Checkin, error) {
	var c Checkin
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM tickets WHERE event_id = $1 AND status IN ('active','used')`, eventID).Scan(&c.TicketsTotal); err != nil {
		return c, err
	}
	err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM checkins ck
		  JOIN tickets t ON t.id = ck.ticket_id
		 WHERE t.event_id = $1 AND NOT ck.is_reentry`, eventID).Scan(&c.Admitted)
	return c, err
}

// Finance resume o financeiro do evento.
type Finance struct {
	GrossCents    int64 `json:"gross_cents"`
	TaxaCents     int64 `json:"taxa_cents"`
	RepasseCents  int64 `json:"repasse_cents"`  // receita líquida do produtor
	RetencaoCents int64 `json:"retencao_cents"` // reserva de contestação (cartão)
	EstornoCents  int64 `json:"estorno_cents"`
}

// EventFinance soma ordens pagas (bruto) e o razão por tipo.
func EventFinance(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (Finance, error) {
	var f Finance
	// Bruto do produtor = valor de FACE (modelo Sympla §4: a taxa é do comprador, não sai
	// do produtor). O repasse é o mesmo face, limpo.
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(face_cents),0) FROM orders WHERE event_id=$1 AND status='paid'`, eventID).Scan(&f.GrossCents); err != nil {
		return f, err
	}
	rows, err := tx.Query(ctx, `
		SELECT kind, COALESCE(SUM(amount_cents),0) FROM ledger_entries WHERE event_id=$1 GROUP BY kind`, eventID)
	if err != nil {
		return f, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var amount int64
		if err := rows.Scan(&kind, &amount); err != nil {
			return f, err
		}
		switch kind {
		case "taxa":
			f.TaxaCents = amount
		case "repasse":
			f.RepasseCents = amount
		case "retencao":
			f.RetencaoCents = amount
		case "estorno":
			f.EstornoCents = amount
		}
	}
	return f, rows.Err()
}

// SessionFunnel é o funil da sessão de checkout por evento: quem chegou a 'authenticated'
// (vinculado) contra quem pagou. Sessão vinculada e não paga vira 'abandoned' na expiração.
type SessionFunnel struct {
	Bound     int `json:"bound"` // chegou a authenticated (paid + abandoned)
	Paid      int `json:"paid"`
	Abandoned int `json:"abandoned"` // vinculada e não paga (expirou após o bind)
}

// EventSessionFunnel agrega as sessões de checkout de um evento.
func EventSessionFunnel(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (SessionFunnel, error) {
	var f SessionFunnel
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE status='paid'),
		       COUNT(*) FILTER (WHERE status='abandoned')
		  FROM checkout_sessions WHERE event_id=$1`, eventID).Scan(&f.Paid, &f.Abandoned); err != nil {
		return f, err
	}
	f.Bound = f.Paid + f.Abandoned
	return f, nil
}

// PlatformSessionFunnel agrega o funil da plataforma inteira (administrativo).
func PlatformSessionFunnel(ctx context.Context, pool *pgxpool.Pool) (SessionFunnel, error) {
	var f SessionFunnel
	schemas, err := tenancy.ListTenantSchemas(ctx, pool)
	if err != nil {
		return f, err
	}
	for _, tid := range schemas {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return f, err
		}
		if err := tenancy.WithTenant(ctx, tx, tid); err != nil {
			tx.Rollback(ctx)
			return f, err
		}
		var paid, ab int
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FILTER (WHERE status='paid'), COUNT(*) FILTER (WHERE status='abandoned')
			  FROM checkout_sessions`).Scan(&paid, &ab); err != nil {
			tx.Rollback(ctx)
			return f, err
		}
		tx.Commit(ctx)
		f.Paid += paid
		f.Abandoned += ab
	}
	f.Bound = f.Paid + f.Abandoned
	return f, nil
}

// ProducerSummary é o resumo do produtor (para o painel-mãe).
type ProducerSummary struct {
	EventsActive   int   `json:"events_active"`
	EventsFinished int   `json:"events_finished"`
	TicketsSold    int   `json:"tickets_sold"`
	GrossCents     int64 `json:"gross_cents"`
}

// Summary agrega o produtor inteiro.
func Summary(ctx context.Context, tx pgx.Tx) (ProducerSummary, error) {
	var s ProducerSummary
	_ = tx.QueryRow(ctx, `SELECT COUNT(*) FROM events WHERE status='published'`).Scan(&s.EventsActive)
	_ = tx.QueryRow(ctx, `SELECT COUNT(*) FROM events WHERE status='finished'`).Scan(&s.EventsFinished)
	_ = tx.QueryRow(ctx, `SELECT COUNT(*) FROM tickets WHERE status IN ('active','used')`).Scan(&s.TicketsSold)
	err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(total_cents),0) FROM orders WHERE status='paid'`).Scan(&s.GrossCents)
	return s, err
}

// TicketRow é uma linha da exportação CSV.
type TicketRow struct {
	TicketID string
	LotName  string
	Seat     string
	Status   string
	Created  string
}

// TicketsForExport lista ingressos do evento para CSV.
func TicketsForExport(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) ([]TicketRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT t.id::text, COALESCE(l.name,''),
		       COALESCE(se.name,'') || CASE WHEN s.row_label IS NOT NULL THEN ' '||s.row_label||s.number ELSE '' END,
		       t.status, to_char(t.created_at, 'YYYY-MM-DD"T"HH24:MI:SS')
		  FROM tickets t
		  LEFT JOIN lots l ON l.id = t.lot_id
		  LEFT JOIN seats s ON s.id = t.seat_id
		  LEFT JOIN sectors se ON se.id = s.sector_id
		 WHERE t.event_id = $1
		 ORDER BY t.created_at`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TicketRow
	for rows.Next() {
		var r TicketRow
		if err := rows.Scan(&r.TicketID, &r.LotName, &r.Seat, &r.Status, &r.Created); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PlatformSummary é o painel administrativo consolidado (plataforma).
type PlatformSummary struct {
	ProducersTotal    int   `json:"producers_total"`
	ProducersPending  int   `json:"producers_pending"`
	EventsActive      int   `json:"events_active"`
	RevenueTodayCents int64 `json:"revenue_today_cents"` // taxa da plataforma hoje (São Paulo)
}

// Platform varre os schemas de produtor e consolida. Read-only.
func Platform(ctx context.Context, pool *pgxpool.Pool) (PlatformSummary, error) {
	var p PlatformSummary
	if err := pool.QueryRow(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE status='pending') FROM public.producers`).Scan(&p.ProducersTotal, &p.ProducersPending); err != nil {
		return p, err
	}
	schemas, err := tenancy.ListTenantSchemas(ctx, pool)
	if err != nil {
		return p, err
	}
	for _, tid := range schemas {
		if err := platformTenant(ctx, pool, tid, &p); err != nil {
			return p, err
		}
	}
	return p, nil
}

func platformTenant(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, p *PlatformSummary) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, tenantID); err != nil {
		return err
	}
	var active int
	var today int64
	_ = tx.QueryRow(ctx, `SELECT COUNT(*) FROM events WHERE status='published'`).Scan(&active)
	_ = tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_cents),0) FROM ledger_entries
		 WHERE kind='taxa'
		   AND created_at >= date_trunc('day', now() AT TIME ZONE 'America/Sao_Paulo')`).Scan(&today)
	p.EventsActive += active
	p.RevenueTodayCents += today
	return tx.Commit(ctx)
}
