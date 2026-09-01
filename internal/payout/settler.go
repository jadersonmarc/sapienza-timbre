package payout

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"
)

// Settler mantém as obrigações de repasse em dia: recalcula os valores e move o repasse de
// 'accruing' para 'pending' quando o evento acontece.
//
// A transição não pode depender de alguém abrir uma tela. Um evento que terminou ontem
// aconteceu, e o produtor precisa ver a data do repasse dele mesmo que ninguém tenha entrado
// no painel desde então.
//
// Este Settler NÃO PAGA NADA. Não há transferência bancária no produto: ele calcula,
// registra e deixa o pagamento como decisão manual do admin, com comprovante.
type Settler struct {
	pool     *pgxpool.Pool
	interval time.Duration
}

// NewSettler constrói o settler (intervalo default 1h).
func NewSettler(pool *pgxpool.Pool) *Settler {
	return &Settler{pool: pool, interval: time.Hour}
}

// Run mantém as obrigações em dia até o contexto encerrar.
func (s *Settler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		if err := s.settleAll(ctx); err != nil {
			slog.Warn("repasse: varredura", "err", err)
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
			slog.Warn("repasse: produtor", "tenant", tid, "err", err)
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
	// Onde transferir. Sem destino cadastrado, um repasse vencido não é "atrasado pela
	// plataforma": está esperando o produtor. A retenção diz isso com todas as letras, em
	// vez de o valor ficar parado sem explicação na tela dele.
	var pixKey string
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(payout_pix_key,'') FROM public.producers WHERE id=$1`, tenantID).Scan(&pixKey); err != nil {
		return err
	}

	ids, err := EventIDs(ctx, tx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		p, err := Recompute(ctx, tx, id)
		if err != nil {
			return err
		}
		switch {
		case pixKey == "" && p.Status == StatusPending && p.NetDueCents > 0:
			if err := Hold(ctx, tx, id, HoldBankPending, "sistema"); err != nil {
				return err
			}
		case pixKey != "" && p.Status == StatusOnHold && p.HoldReason == HoldBankPending:
			// A retenção que o próprio sistema pôs, o próprio sistema tira quando a causa
			// some. Deixar para alguém clicar transformaria um cadastro resolvido em espera.
			if err := Release(ctx, tx, id); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// RecomputeEvent recalcula a obrigação de um evento numa transação própria, escopada no
// produtor. É o atalho de quem já terminou de mexer no dinheiro e precisa que o extrato
// reflita isso na hora, sem esperar a varredura.
func RecomputeEvent(ctx context.Context, pool *pgxpool.Pool, producerID, eventID uuid.UUID) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, producerID); err != nil {
		return err
	}
	if _, err := Recompute(ctx, tx, eventID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
