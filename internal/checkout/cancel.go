package checkout

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Parâmetros PROVISÓRIOS da fila de devolução do cancelamento. Mesmo formato do worker de
// âncora: backoff exponencial com teto, e 'failed' só depois de esgotar as tentativas.
const (
	CancelBackoffBase        = 30 * time.Second
	CancelBackoffCap         = 30 * time.Minute
	DefaultCancelMaxAttempts = 8
)

// CancelJob é uma devolução pendente do cancelamento de um evento.
type CancelJob struct {
	ID       uuid.UUID
	EventID  uuid.UUID
	OrderID  uuid.UUID
	Attempts int
}

// CancelProgress é o andamento do lote, como o painel mostra.
type CancelProgress struct {
	Total   int `json:"total"`
	Done    int `json:"done"`
	Failed  int `json:"failed"`
	Pending int `json:"pending"`
}

// EnqueueEventRefunds cria uma devolução pendente por pedido pago do evento e devolve
// quantas entraram. Idempotente pelo índice único por pedido: cancelar duas vezes não vira
// duas devoluções.
//
// Não estorna nada aqui. Cancelar um evento de mil ingressos numa requisição só significa
// mil chamadas ao gateway com o produtor olhando para uma tela travada — e um timeout no
// meio deixaria metade devolvida sem registro de onde parou.
func EnqueueEventRefunds(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (int, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO refund_jobs (event_id, order_id)
		SELECT $1, o.id FROM orders o
		 WHERE o.event_id = $1
		   AND o.status IN ('paid', 'partially_refunded')
		   AND EXISTS (SELECT 1 FROM tickets t WHERE t.order_id = o.id AND t.status = 'active')
		ON CONFLICT (order_id) DO NOTHING`, eventID)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// MarkAttestationStale marca que o registro canônico do evento precisa ser republicado
// quando o lote acabar — uma vez, não a cada devolução.
func MarkAttestationStale(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		UPDATE events SET attestation_stale = true, updated_at = now()
		 WHERE id = $1 AND EXISTS (SELECT 1 FROM event_attestations WHERE event_id = $1)`, eventID)
	return err
}

// ClaimCancelJobs pega o próximo lote de devoluções pendentes e as marca 'running', numa
// transação curta. FOR UPDATE SKIP LOCKED para duas réplicas não pegarem o mesmo pedido.
func ClaimCancelJobs(ctx context.Context, tx pgx.Tx, limit int) ([]CancelJob, error) {
	rows, err := tx.Query(ctx, `
		UPDATE refund_jobs SET status='running', updated_at=now()
		 WHERE id IN (
		   SELECT id FROM refund_jobs
		    WHERE status='pending' AND next_attempt_at <= now()
		    ORDER BY next_attempt_at
		    LIMIT $1
		    FOR UPDATE SKIP LOCKED)
		RETURNING id, event_id, order_id, attempts`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CancelJob
	for rows.Next() {
		var j CancelJob
		if err := rows.Scan(&j.ID, &j.EventID, &j.OrderID, &j.Attempts); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// FinishCancelJob encerra a devolução com sucesso.
func FinishCancelJob(ctx context.Context, tx pgx.Tx, jobID uuid.UUID, requestID *uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		UPDATE refund_jobs SET status='done', attempts=attempts+1, last_error=NULL,
		       request_id=COALESCE($2, request_id), updated_at=now()
		 WHERE id=$1`, jobID, requestID)
	return err
}

// RetryCancelJob devolve a linha para a fila com backoff, ou a encerra como 'failed' quando
// as tentativas esgotam. Falha esgotada NÃO some: vira trabalho manual, com o motivo à
// vista — dinheiro que não voltou precisa de alguém, não de silêncio.
func RetryCancelJob(ctx context.Context, tx pgx.Tx, jobID uuid.UUID, attempts, maxAttempts int,
	cause string, backoff time.Duration) error {
	if attempts >= maxAttempts {
		_, err := tx.Exec(ctx, `
			UPDATE refund_jobs SET status='failed', attempts=$2, last_error=$3, updated_at=now()
			 WHERE id=$1`, jobID, attempts, cause)
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE refund_jobs SET status='pending', attempts=$2, last_error=$3,
		       next_attempt_at = now() + make_interval(secs => $4), updated_at=now()
		 WHERE id=$1`, jobID, attempts, cause, backoff.Seconds())
	return err
}

// ReopenCancelJob devolve uma falha à fila, zerando as tentativas. É o "tentar de novo" do
// painel, depois que alguém resolveu a causa (saldo na conta, cadastro do comprador).
func ReopenCancelJob(ctx context.Context, tx pgx.Tx, jobID uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		UPDATE refund_jobs SET status='pending', attempts=0, next_attempt_at=now(), updated_at=now()
		 WHERE id=$1 AND status='failed'`, jobID)
	return err
}

// CancelBackoff é o atraso exponencial com teto, no mesmo formato do worker de âncora.
func CancelBackoff(attempts int) time.Duration {
	d := CancelBackoffBase * time.Duration(1<<(attempts-1))
	if d > CancelBackoffCap {
		return CancelBackoffCap
	}
	return d
}

// EventCancelProgress conta o andamento do lote de um evento.
func EventCancelProgress(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (CancelProgress, error) {
	var p CancelProgress
	err := tx.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE status='done'),
		       count(*) FILTER (WHERE status='failed'),
		       count(*) FILTER (WHERE status IN ('pending','running'))
		  FROM refund_jobs WHERE event_id=$1`, eventID).
		Scan(&p.Total, &p.Done, &p.Failed, &p.Pending)
	return p, err
}

// StaleAttestations lista os eventos cujo lote terminou e cujo registro canônico ainda não
// foi republicado. Enquanto houver devolução pendente, o atestado espera: republicar no meio
// do lote geraria uma versão por pedido.
func StaleAttestations(ctx context.Context, tx pgx.Tx) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
		SELECT e.id FROM events e
		 WHERE e.attestation_stale
		   AND NOT EXISTS (
		     SELECT 1 FROM refund_jobs j
		      WHERE j.event_id = e.id AND j.status IN ('pending','running'))`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// IsAttestationStale diz se um LOTE já é dono da republicação deste evento. Enquanto for,
// o estorno avulso não republica: cada devolução do lote geraria uma versão, e o registro
// canônico viraria ruído em vez de prova.
func IsAttestationStale(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (bool, error) {
	var stale bool
	err := tx.QueryRow(ctx, `SELECT attestation_stale FROM events WHERE id=$1`, eventID).Scan(&stale)
	return stale, err
}

// ClearAttestationStale desmarca o evento depois de republicar.
func ClearAttestationStale(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE events SET attestation_stale=false WHERE id=$1`, eventID)
	return err
}

// CancelledOrderBuyers lista os compradores a avisar do cancelamento. O aviso sai na
// TRANSIÇÃO, não no fim da devolução: quem tinha ingresso para amanhã precisa saber hoje,
// mesmo que o dinheiro leve dias para voltar.
func CancelledOrderBuyers(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) ([]string, string, error) {
	var title string
	if err := tx.QueryRow(ctx, `SELECT title FROM events WHERE id=$1`, eventID).Scan(&title); err != nil {
		return nil, "", err
	}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT o.buyer_email FROM orders o
		 WHERE o.event_id=$1 AND o.buyer_email IS NOT NULL AND o.buyer_email <> ''
		   AND o.status IN ('paid','partially_refunded')`, eventID)
	if err != nil {
		return nil, title, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, title, err
		}
		out = append(out, e)
	}
	return out, title, rows.Err()
}
