-- Outbox de notificação.
--
-- Dois defeitos, um mecanismo. (1) A mensagem era gravada pelo pool, FORA da transação da
-- venda: um rollback tardio mandava o ingresso de uma compra que não existe. (2) O worker
-- fazia FOR UPDATE SKIP LOCKED fora de transação, então o lock soltava na hora e duas
-- réplicas entregavam a mesma mensagem.
--
-- A gravação passa a ser na transação de quem chama, e o worker passa a segurar o lock pelo
-- tempo do processamento.

-- Chave de idempotência da MENSAGEM: impede que o mesmo aviso seja enfileirado duas vezes
-- quando o caminho que o produz é retentado (webhook reprocessado, worker que volta). Nula
-- para os avisos que legitimamente se repetem — um novo código de acesso é outro código.
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS idempotency_key text;
CREATE UNIQUE INDEX IF NOT EXISTS notifications_idempotency_key
    ON notifications (idempotency_key) WHERE idempotency_key IS NOT NULL;

-- Quando a mensagem volta a ficar elegível. Antes o worker usava `updated_at <= now()` como
-- agenda, o que faz "recém-mexida" significar "pronta" — e mistura duas coisas na mesma
-- coluna. O backoff agora tem lugar próprio, como no worker de âncora.
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS next_attempt_at timestamptz NOT NULL DEFAULT now();
UPDATE notifications SET next_attempt_at = updated_at WHERE status = 'queued';

CREATE INDEX IF NOT EXISTS notifications_ready_idx
    ON notifications (next_attempt_at) WHERE status = 'queued';
