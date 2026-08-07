package chain

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"
)

// EnqueueMint enfileira a emissão on-chain de um ingresso e marca chain_status=pending.
// Roda na MESMA transação da emissão, mas o mint em si acontece depois, em segundo
// plano — a venda nunca espera a rede (guardrail).
func EnqueueMint(ctx context.Context, tx pgx.Tx, ticketID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `INSERT INTO chain_jobs (ticket_id, kind) VALUES ($1,'mint')`, ticketID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE tickets SET chain_status='pending', updated_at=now() WHERE id=$1 AND chain_status='none'`, ticketID)
	return err
}

type job struct {
	id, ticketID   uuid.UUID
	eventID, lotID uuid.UUID
	seatID         *uuid.UUID
	attempts, max  int
	ownerAddr      *string
}

// tokenID: pista = um id por lote (emissão em lote); assento marcado = por evento+setor+
// assento. O ERC-1155 cobre os dois.
func (j job) tokenID() string {
	if j.seatID == nil {
		return "lot:" + j.lotID.String()
	}
	return "seat:" + j.eventID.String() + ":" + j.seatID.String()
}

// ProcessTenant drena os mint-jobs pendentes de um produtor. Claim (FOR UPDATE SKIP
// LOCKED) e marcação 'running' são feitos numa tx curta; o Mint (rede) roda FORA de
// transação; a atualização final vem em outra tx. Assim nenhuma tx fica aberta durante
// a chamada de rede. Devolve quantos foram mintados com sucesso.
func ProcessTenant(ctx context.Context, pool *pgxpool.Pool, driver ChainDriver, tenantID uuid.UUID) (int, error) {
	jobs, err := claim(ctx, pool, tenantID, 20)
	if err != nil {
		return 0, err
	}
	minted := 0
	for _, j := range jobs {
		res, mErr := driver.Mint(ctx, MintRequest{TokenID: j.tokenID(), ToAddress: deref(j.ownerAddr), Amount: 1})
		if err := finish(ctx, pool, tenantID, j, res, mErr); err != nil {
			return minted, err
		}
		if mErr == nil {
			minted++
		}
	}
	return minted, nil
}

func claim(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, limit int) ([]job, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, tenantID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT j.id, j.ticket_id, j.attempts, j.max_attempts,
		       t.event_id, t.lot_id, t.seat_id, w.address
		  FROM chain_jobs j
		  JOIN tickets t ON t.id = j.ticket_id
		  LEFT JOIN public.wallets w ON w.id = t.owner_wallet_id
		 WHERE j.kind='mint' AND j.status='pending' AND j.next_attempt_at <= now()
		 ORDER BY j.next_attempt_at
		 FOR UPDATE OF j SKIP LOCKED
		 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.ticketID, &j.attempts, &j.max, &j.eventID, &j.lotID, &j.seatID, &j.ownerAddr); err != nil {
			rows.Close()
			return nil, err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if _, err := tx.Exec(ctx, `UPDATE chain_jobs SET status='running', updated_at=now() WHERE id=$1`, j.id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return jobs, nil
}

func finish(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, j job, res MintResult, mErr error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, tenantID); err != nil {
		return err
	}
	if mErr != nil {
		attempts := j.attempts + 1
		if attempts >= j.max {
			if _, err := tx.Exec(ctx, `UPDATE chain_jobs SET status='failed', attempts=$2, last_error=$3, updated_at=now() WHERE id=$1`, j.id, attempts, mErr.Error()); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE tickets SET chain_status='failed', updated_at=now() WHERE id=$1`, j.ticketID); err != nil {
				return err
			}
		} else {
			// Backoff linear (15s * tentativa). Volta a 'pending' para nova tentativa.
			if _, err := tx.Exec(ctx, `
				UPDATE chain_jobs SET status='pending', attempts=$2, last_error=$3,
				       next_attempt_at = now() + ($2::int * interval '15 seconds'), updated_at=now()
				 WHERE id=$1`, j.id, attempts, mErr.Error()); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}
	tokenID := res.TokenID
	if tokenID == "" {
		tokenID = j.tokenID()
	}
	if _, err := tx.Exec(ctx, `UPDATE chain_jobs SET status='done', attempts=attempts+1, updated_at=now() WHERE id=$1`, j.id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE tickets SET chain_status='minted', chain_token_id=$2, updated_at=now() WHERE id=$1`, j.ticketID, tokenID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Worker processa a fila de emissão de todos os produtores, periodicamente.
type Worker struct {
	pool     *pgxpool.Pool
	driver   ChainDriver
	interval time.Duration
}

// NewWorker constrói o worker (intervalo default 10s).
func NewWorker(pool *pgxpool.Pool, driver ChainDriver) *Worker {
	return &Worker{pool: pool, driver: driver, interval: 10 * time.Second}
}

// Run processa até o contexto encerrar. No-op enquanto o driver não está habilitado.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if w.driver.Enabled() {
			if err := w.processAll(ctx); err != nil {
				slog.Warn("chain worker", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) processAll(ctx context.Context) error {
	schemas, err := tenancy.ListTenantSchemas(ctx, w.pool)
	if err != nil {
		return err
	}
	for _, tid := range schemas {
		if _, err := ProcessTenant(ctx, w.pool, w.driver, tid); err != nil {
			slog.Warn("chain worker tenant", "tenant", tid, "err", err)
		}
	}
	return nil
}
