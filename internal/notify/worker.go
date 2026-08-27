package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Parâmetros PROVISÓRIOS do worker de envio: backoff exponencial com teto.
const (
	backoffBase        = 15 * time.Second
	backoffCap         = 10 * time.Minute
	DefaultMaxAttempts = 5
)

// Worker drena a fila de public.notifications e envia pelo provider. Falha retryable
// (5xx/rate limit) volta com backoff; falha permanente (4xx) marca 'failed'.
type Worker struct {
	pool     *pgxpool.Pool
	provider Provider
	interval time.Duration
	// batch é quantas mensagens uma passada segura por vez. Pequeno de propósito: a
	// transação fica aberta durante os envios, e um lote grande a mantém aberta demais.
	batch       int
	MaxAttempts int
	Backoff     func(attempts int) time.Duration
}

// NewWorker constrói o worker de notificações.
func NewWorker(pool *pgxpool.Pool, provider Provider) *Worker {
	return &Worker{
		pool: pool, provider: provider, interval: 5 * time.Second, batch: 20,
		MaxAttempts: DefaultMaxAttempts, Backoff: backoff,
	}
}

// Run processa a fila até o contexto encerrar.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.Process(ctx); err != nil {
			slog.Warn("notify worker", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Process faz uma passada pela fila (exposto para testes).
//
// O lock vive o tempo do PROCESSAMENTO: a seleção com FOR UPDATE SKIP LOCKED acontece
// dentro da transação, o envio acontece com ela aberta, e só então há commit. Antes a
// seleção era pelo pool — o lock soltava na mesma hora e duas réplicas entregavam a mesma
// mensagem.
//
// O preço é segurar a transação durante uma chamada HTTP, e é o preço certo aqui: o lote é
// pequeno e a alternativa é e-mail duplicado. Resta uma janela conhecida — envio bem
// sucedido com commit falho reenvia —, que é o comportamento "ao menos uma vez" desta
// classe de fila; a chave de idempotência da mensagem cobre a duplicação na ENTRADA.
func (w *Worker) Process(ctx context.Context) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT id, to_email, payload, attempts FROM public.notifications
		 WHERE status='queued' AND next_attempt_at <= now()
		 ORDER BY next_attempt_at LIMIT $1 FOR UPDATE SKIP LOCKED`, w.batch)
	if err != nil {
		return err
	}
	var list []queued
	for rows.Next() {
		var it queued
		if err := rows.Scan(&it.id, &it.to, &it.payload, &it.attempts); err != nil {
			rows.Close()
			return err
		}
		list = append(list, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, it := range list {
		if err := w.processOne(ctx, tx, it); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

type queued struct {
	id       uuid.UUID
	to       string
	payload  []byte
	attempts int
}

func (w *Worker) processOne(ctx context.Context, tx pgx.Tx, it queued) error {
	var m RenderedMessage
	if err := json.Unmarshal(it.payload, &m); err != nil {
		return err
	}
	providerID, err := w.provider.Send(ctx, m)
	if err == nil {
		_, uerr := tx.Exec(ctx, `
			UPDATE public.notifications SET status='sent', provider_message_id=$2, attempts=attempts+1,
			       sent_at=now(), updated_at=now()
			 WHERE id=$1`, it.id, providerID)
		if uerr == nil {
			slog.Info("notify: enviado", "id", it.id, "to", it.to, "provider_message_id", providerID)
		}
		return uerr
	}
	// Mesmo formato do worker de âncora: tentativas, backoff exponencial com teto e o
	// motivo persistido; 'failed' só depois de esgotar.
	attempts := it.attempts + 1
	if isRetryable(err) && attempts < w.MaxAttempts {
		slog.Warn("notify: erro retryable, reenfileirado", "id", it.id, "to", it.to, "attempts", attempts, "err", err.Error())
		_, uerr := tx.Exec(ctx, `
			UPDATE public.notifications SET status='queued', attempts=$2, last_error=$3,
			       next_attempt_at = now() + make_interval(secs => $4), updated_at=now()
			 WHERE id=$1`, it.id, attempts, err.Error(), w.Backoff(attempts).Seconds())
		return uerr
	}
	slog.Warn("notify: falhou", "id", it.id, "to", it.to, "attempts", attempts, "err", err.Error())
	_, uerr := tx.Exec(ctx, `
		UPDATE public.notifications SET status='failed', attempts=$2, last_error=$3, updated_at=now()
		 WHERE id=$1`, it.id, attempts, err.Error())
	return uerr
}

// ResendNotification reenfileira uma notificação (painel do produtor) criando um NOVO
// registro de envio para a mesma mensagem — nunca duplica o ingresso, só a tentativa de
// entrega. Devolve o id do novo registro.
func ResendNotification(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) (uuid.UUID, error) {
	var newID uuid.UUID
	err := pool.QueryRow(ctx, `
		INSERT INTO public.notifications (producer_id, event_id, kind, to_email, subject_id, ticket_id, order_id, payload)
		SELECT producer_id, event_id, kind, to_email, subject_id, ticket_id, order_id, payload
		  FROM public.notifications WHERE id=$1
		-- Sem idempotency_key: o reenvio é outra TENTATIVA de entrega da mesma mensagem, e
		-- herdar a chave faria o ON CONFLICT engolir justamente o reenvio pedido à mão.
		RETURNING id`, id).Scan(&newID)
	return newID, err
}

// backoff devolve o atraso exponencial com teto.
func backoff(attempts int) time.Duration {
	d := backoffBase * time.Duration(1<<(attempts-1))
	if d > backoffCap {
		return backoffCap
	}
	return d
}
