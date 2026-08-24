package attest

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-timbre/internal/chain"
	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
)

// DefaultCloseAfter é o prazo padrão para o fechamento automático após ends_at. PROVISÓRIO.
const DefaultCloseAfter = 24 * time.Hour

// Parâmetros PROVISÓRIOS do worker de âncora: backoff exponencial com teto.
const (
	anchorBackoffBase        = 15 * time.Second
	anchorBackoffCap         = 10 * time.Minute
	DefaultAnchorMaxAttempts = 10
)

// Closer fecha eventos automaticamente N horas após ends_at.
type Closer struct {
	pool       *pgxpool.Pool
	signer     *ticketing.Signer
	anchorer   chain.Anchorer
	anchorMode chain.AnchorMode
	keyID      string
	after      time.Duration
	interval   time.Duration
}

// NewCloser constrói o fechador automático.
func NewCloser(pool *pgxpool.Pool, signer *ticketing.Signer, anchorer chain.Anchorer, anchorMode chain.AnchorMode, keyID string, after time.Duration) *Closer {
	if after <= 0 {
		after = DefaultCloseAfter
	}
	return &Closer{pool: pool, signer: signer, anchorer: anchorer, anchorMode: anchorMode, keyID: keyID, after: after, interval: 10 * time.Minute}
}

// Run fecha eventos vencidos até o contexto encerrar.
func (c *Closer) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		if err := c.processAll(ctx); err != nil {
			slog.Warn("attest closer", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Closer) processAll(ctx context.Context) error {
	schemas, err := tenancy.ListTenantSchemas(ctx, c.pool)
	if err != nil {
		return err
	}
	for _, tid := range schemas {
		if err := c.processTenant(ctx, tid); err != nil {
			slog.Warn("attest closer tenant", "tenant", tid, "err", err)
		}
	}
	return nil
}

func (c *Closer) processTenant(ctx context.Context, tenantID uuid.UUID) error {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, tenantID); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
		SELECT id FROM events e
		 WHERE e.ends_at IS NOT NULL
		   AND e.ends_at < now() - make_interval(secs => $1)
		   AND NOT EXISTS (SELECT 1 FROM event_attestations a WHERE a.event_id=e.id AND a.supersedes_id IS NULL)
		   AND e.status <> 'cancelled'`, c.after.Seconds())
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := Close(ctx, tx, c.signer, c.anchorer, c.anchorMode, c.keyID, tenantID, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// AnchorWorker processa os jobs de âncora (chain_jobs kind='anchor') com backoff
// exponencial. 'failed' só após esgotar as tentativas, com o motivo persistido. A âncora
// NUNCA bloqueia o fechamento.
type AnchorWorker struct {
	pool        *pgxpool.Pool
	anchorer    chain.Anchorer
	interval    time.Duration
	MaxAttempts int
	Backoff     func(attempts int) time.Duration
}

// NewAnchorWorker constrói o worker de âncora.
func NewAnchorWorker(pool *pgxpool.Pool, anchorer chain.Anchorer) *AnchorWorker {
	return &AnchorWorker{
		pool: pool, anchorer: anchorer, interval: 10 * time.Second,
		MaxAttempts: DefaultAnchorMaxAttempts, Backoff: anchorBackoff,
	}
}

// Run processa âncoras pendentes até o contexto encerrar (no-op se o anchorer não habilita).
func (w *AnchorWorker) Run(ctx context.Context) {
	if !w.anchorer.Enabled() {
		return // off/log: nada processa — nada vira 'anchored' sem transação real.
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.processAll(ctx); err != nil {
			slog.Warn("anchor worker", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ProcessTenant processa as âncoras pendentes de um tenant (exposto para testes).
func (w *AnchorWorker) ProcessTenant(ctx context.Context, tenantID uuid.UUID) error {
	return w.processTenant(ctx, tenantID)
}

func (w *AnchorWorker) processAll(ctx context.Context) error {
	schemas, err := tenancy.ListTenantSchemas(ctx, w.pool)
	if err != nil {
		return err
	}
	for _, tid := range schemas {
		if err := w.processTenant(ctx, tid); err != nil {
			slog.Warn("anchor worker tenant", "tenant", tid, "err", err)
		}
	}
	return nil
}

func (w *AnchorWorker) processTenant(ctx context.Context, tenantID uuid.UUID) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, tenantID); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
		SELECT j.id, j.attestation_id, j.attempts, a.digest
		  FROM chain_jobs j JOIN event_attestations a ON a.id = j.attestation_id
		 WHERE j.kind='anchor' AND j.status='pending' AND j.next_attempt_at <= now()
		 ORDER BY j.next_attempt_at LIMIT 20
		 FOR UPDATE OF j SKIP LOCKED`)
	if err != nil {
		return err
	}
	type job struct {
		id, attID uuid.UUID
		attempts  int
		digest    []byte
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.attID, &j.attempts, &j.digest); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, j := range jobs {
		if err := w.processOne(ctx, tx, j); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (w *AnchorWorker) processOne(ctx context.Context, tx pgx.Tx, j struct {
	id, attID uuid.UUID
	attempts  int
	digest    []byte
}) error {
	txHash, aerr := w.anchorer.SendAnchor(ctx, j.digest)
	if aerr == nil {
		if _, err := tx.Exec(ctx, `
			UPDATE chain_jobs SET status='done', attempts=attempts+1, tx_hash=$2, updated_at=now() WHERE id=$1`,
			j.id, nilStr(txHash)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE event_attestations SET anchor_status='anchored', anchor_tx_hash=$2, anchored_at=now() WHERE id=$1`,
			j.attID, nilStr(txHash))
		return err
	}
	// Falha: tenta de novo até esgotar maxAttempts (backoff exponencial), depois 'failed'.
	attempts := j.attempts + 1
	if attempts >= w.MaxAttempts {
		if _, err := tx.Exec(ctx, `
			UPDATE chain_jobs SET status='failed', attempts=$2, last_error=$3, updated_at=now() WHERE id=$1`,
			j.id, attempts, aerr.Error()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE event_attestations SET anchor_status='failed' WHERE id=$1`, j.attID)
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE chain_jobs SET status='pending', attempts=$2, last_error=$3,
		       next_attempt_at = now() + make_interval(secs => $4), updated_at=now()
		 WHERE id=$1`, j.id, attempts, aerr.Error(), w.Backoff(attempts).Seconds()); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE event_attestations SET anchor_status='pending' WHERE id=$1`, j.attID)
	return err
}

// anchorBackoff devolve o atraso exponencial com teto.
func anchorBackoff(attempts int) time.Duration {
	d := anchorBackoffBase * time.Duration(1<<(attempts-1))
	if d > anchorBackoffCap {
		return anchorBackoffCap
	}
	return d
}

func nilStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
