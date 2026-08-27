-- Cancelamento de evento com devolução em massa.
--
-- Até aqui cancelar era só mudar o status: o evento sumia do diretório e todo mundo ficava
-- com ingresso válido e dinheiro pago. Era o pior estado possível do sistema — o produtor
-- achava que tinha resolvido, e cada comprador descobria sozinho.
--
-- A devolução de centenas de pedidos não cabe numa requisição: o gateway responde um por
-- vez, qualquer um pode falhar, e um timeout no meio deixaria metade devolvida sem registro
-- de onde parou. Por isso vira fila.
CREATE TABLE IF NOT EXISTS refund_jobs (
    id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    order_id uuid NOT NULL REFERENCES orders(id) ON DELETE CASCADE,

    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'running', 'done', 'failed')),
    attempts        int NOT NULL DEFAULT 0,
    last_error      text,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),

    -- O pedido de estorno criado para esta ordem, quando chegou a existir. É por ele que a
    -- trilha de auditoria do cancelamento se liga à de cada devolução.
    request_id uuid REFERENCES refund_requests(id),

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- INVARIANTE (por índice): uma devolução enfileirada por pedido. Cancelar duas vezes — dois
-- cliques, ou um retry do painel — não pode virar duas devoluções.
CREATE UNIQUE INDEX IF NOT EXISTS refund_jobs_order_key ON refund_jobs (order_id);
CREATE INDEX IF NOT EXISTS refund_jobs_pending_idx
    ON refund_jobs (next_attempt_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS refund_jobs_event_idx ON refund_jobs (event_id, status);

-- Marca que o atestado do evento precisa ser republicado quando o lote terminar. Republicar
-- a cada devolução geraria uma versão por pedido — e o registro canônico viraria ruído em
-- vez de prova.
ALTER TABLE events ADD COLUMN IF NOT EXISTS attestation_stale boolean NOT NULL DEFAULT false;
