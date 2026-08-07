package inventory

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"
)

// Sweeper roda a varredura de expiração de holds em todos os schemas de produtor,
// periodicamente. A fonte de verdade da expiração é o Postgres (nunca Redis).
type Sweeper struct {
	pool     *pgxpool.Pool
	interval time.Duration
}

// NewSweeper constrói a varredura (intervalo default 30s).
func NewSweeper(pool *pgxpool.Pool) *Sweeper {
	return &Sweeper{pool: pool, interval: 30 * time.Second}
}

// WithInterval ajusta o intervalo.
func (s *Sweeper) WithInterval(d time.Duration) *Sweeper {
	s.interval = d
	return s
}

// Run varre até o contexto encerrar. Bloqueia; chame em uma goroutine.
func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		if err := s.SweepAll(ctx); err != nil {
			slog.Warn("sweep de holds", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// SweepAll expira holds vencidos em cada schema de produtor. Uma tx por tenant.
func (s *Sweeper) SweepAll(ctx context.Context) error {
	schemas, err := tenancy.ListTenantSchemas(ctx, s.pool)
	if err != nil {
		return err
	}
	for _, tid := range schemas {
		if err := s.sweepTenant(ctx, tid); err != nil {
			slog.Warn("sweep tenant", "tenant", tid, "err", err)
		}
	}
	return nil
}

func (s *Sweeper) sweepTenant(ctx context.Context, tenantID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, tenantID); err != nil {
		return err
	}
	if _, err := ExpireDue(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
