package chain

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"
)

// EnqueueMint enfileira a emissão on-chain de um ingresso e marca chain_status=pending.
// Roda na MESMA transação da emissão, mas o mint em si acontece depois, em segundo
// plano — a venda nunca espera a rede (guardrail). É o caminho EAGER (CHAIN_MINT_MODE).
func EnqueueMint(ctx context.Context, tx pgx.Tx, ticketID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `INSERT INTO chain_jobs (ticket_id, kind) VALUES ($1,'mint')`, ticketID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE tickets SET chain_status='pending', updated_at=now() WHERE id=$1 AND chain_status='not_materialized'`, ticketID)
	return err
}

type job struct {
	id, ticketID   uuid.UUID
	kind           string
	eventID, lotID uuid.UUID
	seatID         *uuid.UUID
	attempts, max  int
	amount         int
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
	done := 0
	for _, j := range jobs {
		if j.kind == "transfer" {
			ok, err := processTransfer(ctx, pool, driver, tenantID, j)
			if err != nil {
				return done, err
			}
			if ok {
				done++
			}
			continue
		}
		res, mErr := driver.Mint(ctx, MintRequest{TokenID: j.tokenID(), ToAddress: deref(j.ownerAddr), Amount: int64(j.amount)})
		if err := finish(ctx, pool, tenantID, j, res, mErr); err != nil {
			return done, err
		}
		if mErr == nil {
			done++
		}
	}
	return done, nil
}

// processTransfer executa a transferência on-chain do último transfer pendente do
// ingresso e confirma a linha em transfers. from/to vêm do próprio transfer.
func processTransfer(ctx context.Context, pool *pgxpool.Pool, driver ChainDriver, tenantID uuid.UUID, j job) (bool, error) {
	var transferID uuid.UUID
	var fromAddr, toAddr *string
	var price int64
	err := readTenant(ctx, pool, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT tr.id, fw.address, tw.address, tr.price_cents
			  FROM transfers tr
			  LEFT JOIN public.wallets fw ON fw.id = tr.from_wallet_id
			  LEFT JOIN public.wallets tw ON tw.id = tr.to_wallet_id
			 WHERE tr.ticket_id = $1 AND tr.status = 'pending'
			 ORDER BY tr.created_at DESC LIMIT 1`, j.ticketID).Scan(&transferID, &fromAddr, &toAddr, &price)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Nada pendente: encerra o job sem ação.
		return false, markJobDone(ctx, pool, tenantID, j.id)
	}
	if err != nil {
		return false, err
	}
	res, mErr := driver.Transfer(ctx, TransferRequest{
		TokenID: j.tokenID(), FromAddress: deref(fromAddr), ToAddress: deref(toAddr), PriceCents: price,
	})
	if mErr != nil {
		return false, backoffJob(ctx, pool, tenantID, j, mErr)
	}
	return true, finishTransfer(ctx, pool, tenantID, j.id, transferID, res.TxHash)
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
		SELECT j.id, j.ticket_id, j.kind, j.attempts, j.max_attempts, j.amount,
		       t.event_id, t.lot_id, t.seat_id, w.address
		  FROM chain_jobs j
		  JOIN tickets t ON t.id = j.ticket_id
		  LEFT JOIN public.wallets w ON w.id = t.owner_wallet_id
		 WHERE j.kind IN ('mint','transfer') AND j.status='pending' AND j.next_attempt_at <= now()
		 ORDER BY j.next_attempt_at
		 FOR UPDATE OF j SKIP LOCKED
		 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.ticketID, &j.kind, &j.attempts, &j.max, &j.amount, &j.eventID, &j.lotID, &j.seatID, &j.ownerAddr); err != nil {
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
	if _, err := tx.Exec(ctx, `
		UPDATE chain_jobs SET status='done', attempts=attempts+1, tx_hash=$2, gas_cost_wei=$3, updated_at=now()
		 WHERE id=$1`, j.id, nilStr(res.TxHash), gasOrNil(res.GasCostWei)); err != nil {
		return err
	}
	// Ingressos afetados: pista agrupada (todo o lote pendente) ou assento individual.
	var affected []uuid.UUID
	if j.seatID == nil {
		rows, err := tx.Query(ctx, `
			SELECT id FROM tickets WHERE event_id=$1 AND lot_id=$2 AND seat_id IS NULL AND chain_status='pending'`,
			j.eventID, j.lotID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			affected = append(affected, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	} else {
		affected = []uuid.UUID{j.ticketID}
	}
	if _, err := tx.Exec(ctx, `UPDATE tickets SET chain_status='minted', chain_token_id=$2, updated_at=now() WHERE id = ANY($1)`, affected, tokenID); err != nil {
		return err
	}
	// Projeta no índice público (meus ingressos).
	if _, err := tx.Exec(ctx, `UPDATE public.ticket_directory SET chain_status='minted' WHERE ticket_id = ANY($1)`, affected); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// gasOrNil converte gás 0 (não medido) em NULL.
func gasOrNil(wei int64) *int64 {
	if wei <= 0 {
		return nil
	}
	return &wei
}

// readTenant roda fn numa tx read-only escopada ao tenant.
func readTenant(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, tenantID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func markJobDone(ctx context.Context, pool *pgxpool.Pool, tenantID, jobID uuid.UUID) error {
	return readTenant(ctx, pool, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE chain_jobs SET status='done', updated_at=now() WHERE id=$1`, jobID)
		return err
	})
}

// backoffJob registra a falha de um job (attempts++, failed ou pending+backoff), sem
// tocar no ingresso (usado pela transferência).
func backoffJob(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, j job, mErr error) error {
	return readTenant(ctx, pool, tenantID, func(tx pgx.Tx) error {
		attempts := j.attempts + 1
		if attempts >= j.max {
			_, err := tx.Exec(ctx, `UPDATE chain_jobs SET status='failed', attempts=$2, last_error=$3, updated_at=now() WHERE id=$1`, j.id, attempts, mErr.Error())
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE chain_jobs SET status='pending', attempts=$2, last_error=$3,
			       next_attempt_at = now() + ($2::int * interval '15 seconds'), updated_at=now()
			 WHERE id=$1`, j.id, attempts, mErr.Error())
		return err
	})
}

// finishTransfer confirma a transferência on-chain e encerra o job.
func finishTransfer(ctx context.Context, pool *pgxpool.Pool, tenantID, jobID, transferID uuid.UUID, txHash string) error {
	return readTenant(ctx, pool, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE transfers SET status='confirmed', tx_hash=$2 WHERE id=$1`, transferID, nilStr(txHash)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE chain_jobs SET status='done', attempts=attempts+1, updated_at=now() WHERE id=$1`, jobID)
		return err
	})
}

func nilStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
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
