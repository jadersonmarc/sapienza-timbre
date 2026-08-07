// Package ledger materializa o repasse ao produtor em payouts. Regras (Etapa 1.8):
// repasse fica disponível D+2 após o evento; a retenção de 5% (cartão) segura o valor
// por 60 dias como reserva de contestação; estornos abatem. O que sobra disponível e
// ainda não foi pago vira um payout. As funções tenant-scoped recebem uma pgx.Tx sob
// tenancy.WithTenant; o Settler varre os produtores pelo pool.
package ledger

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"
)

// NetDue calcula o valor líquido disponível para repasse agora: repasse já liberado,
// menos a retenção ainda retida, mais estornos (negativos), menos o que já foi pago.
func NetDue(ctx context.Context, tx pgx.Tx) (int64, error) {
	var net int64
	err := tx.QueryRow(ctx, `
		SELECT
		  COALESCE((SELECT SUM(amount_cents) FROM ledger_entries
		             WHERE kind='repasse' AND (available_at IS NULL OR available_at <= now())),0)
		- COALESCE((SELECT SUM(amount_cents) FROM ledger_entries
		             WHERE kind='retencao' AND available_at IS NOT NULL AND available_at > now()),0)
		+ COALESCE((SELECT SUM(amount_cents) FROM ledger_entries WHERE kind='estorno'),0)
		- COALESCE((SELECT SUM(amount_cents) FROM payouts WHERE status IN ('pending','sent')),0)`).Scan(&net)
	return net, err
}

// SettleDue cria um payout com o líquido disponível, se positivo. Idempotente na
// prática (o já-pago é subtraído em NetDue). Devolve o valor materializado.
func SettleDue(ctx context.Context, tx pgx.Tx) (int64, error) {
	net, err := NetDue(ctx, tx)
	if err != nil {
		return 0, err
	}
	if net <= 0 {
		return 0, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO payouts (amount_cents, status, scheduled_for)
		VALUES ($1, 'pending', now())`, net); err != nil {
		return 0, err
	}
	return net, nil
}

// Settler roda o fechamento de payouts em todos os produtores, periodicamente.
type Settler struct {
	pool     *pgxpool.Pool
	interval time.Duration
}

// NewSettler constrói o settler (intervalo default 1h).
func NewSettler(pool *pgxpool.Pool) *Settler {
	return &Settler{pool: pool, interval: time.Hour}
}

// Run fecha payouts até o contexto encerrar.
func (s *Settler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		if err := s.settleAll(ctx); err != nil {
			slog.Warn("settler", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Settler) settleAll(ctx context.Context) error {
	schemas, err := tenancy.ListTenantSchemas(ctx, s.pool)
	if err != nil {
		return err
	}
	for _, tid := range schemas {
		if err := s.settleTenant(ctx, tid); err != nil {
			slog.Warn("settler tenant", "tenant", tid, "err", err)
		}
	}
	return nil
}

func (s *Settler) settleTenant(ctx context.Context, tenantID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, tenantID); err != nil {
		return err
	}
	if _, err := SettleDue(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
