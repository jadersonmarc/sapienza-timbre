package dash

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"
)

// PlatformEvent é um evento de qualquer produtor (painel admin /admin/events).
type PlatformEvent struct {
	ProducerID uuid.UUID  `json:"producer_id"`
	Producer   string     `json:"producer"`
	EventID    uuid.UUID  `json:"event_id"`
	Title      string     `json:"title"`
	Category   string     `json:"category"`
	Status     string     `json:"status"`
	StartsAt   *time.Time `json:"starts_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// PlatformEvents varre os schemas de produtor e lista os eventos (read-only).
func PlatformEvents(ctx context.Context, pool *pgxpool.Pool) ([]PlatformEvent, error) {
	names, err := producerNames(ctx, pool)
	if err != nil {
		return nil, err
	}
	schemas, err := tenancy.ListTenantSchemas(ctx, pool)
	if err != nil {
		return nil, err
	}
	var out []PlatformEvent
	for _, tid := range schemas {
		evs, err := tenantEvents(ctx, pool, tid, names[tid])
		if err != nil {
			return nil, err
		}
		out = append(out, evs...)
	}
	return out, nil
}

func tenantEvents(ctx context.Context, pool *pgxpool.Pool, tid uuid.UUID, producer string) ([]PlatformEvent, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, tid); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT id, title, category, status, starts_at, created_at
		  FROM events ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlatformEvent
	for rows.Next() {
		var e PlatformEvent
		e.ProducerID = tid
		e.Producer = producer
		if err := rows.Scan(&e.EventID, &e.Title, &e.Category, &e.Status, &e.StartsAt, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, tx.Commit(ctx)
}

// PlatformSales consolida vendas da plataforma inteira (relatório financeiro).
type PlatformSales struct {
	TicketsSold int   `json:"tickets_sold"`
	GrossCents  int64 `json:"gross_cents"`
	FaceCents   int64 `json:"face_cents"`
	PlatformFee int64 `json:"platform_fee_cents"`
}

// SalesPlatform agrega as vendas por produtor e soma (read-only).
func SalesPlatform(ctx context.Context, pool *pgxpool.Pool) (PlatformSales, error) {
	var sum PlatformSales
	schemas, err := tenancy.ListTenantSchemas(ctx, pool)
	if err != nil {
		return sum, err
	}
	for _, tid := range schemas {
		if err := tenantSales(ctx, pool, tid, &sum); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

func tenantSales(ctx context.Context, pool *pgxpool.Pool, tid uuid.UUID, sum *PlatformSales) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, tid); err != nil {
		return err
	}
	var sold int
	var gross, face, fee int64
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM tickets WHERE status IN ('active','used')`).Scan(&sold); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(total_cents),0), COALESCE(SUM(face_cents),0), COALESCE(SUM(platform_fee_cents),0)
		  FROM orders WHERE status='paid'`).Scan(&gross, &face, &fee); err != nil {
		return err
	}
	sum.TicketsSold += sold
	sum.GrossCents += gross
	sum.FaceCents += face
	sum.PlatformFee += fee
	return tx.Commit(ctx)
}

// producerNames devolve o mapa id -> nome dos produtores (uma consulta só).
func producerNames(ctx context.Context, pool *pgxpool.Pool) (map[uuid.UUID]string, error) {
	rows, err := pool.Query(ctx, `SELECT id, name FROM public.producers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		m[id] = name
	}
	return m, rows.Err()
}
