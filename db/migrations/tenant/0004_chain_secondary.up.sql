-- Rede e mercado secundário por produtor. Todo o sistema fala com a interface
-- ChainDriver (NoopChainDriver por default); estas tabelas são a fila e o registro,
-- não sabem se há rede por trás.

-- chain_jobs — fila de emissão em rede, com tentativas, backoff e último erro.
CREATE TABLE IF NOT EXISTS chain_jobs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_id       uuid REFERENCES tickets(id) ON DELETE CASCADE,
    kind            text NOT NULL CHECK (kind IN ('mint', 'transfer', 'burn')),
    attempts        integer NOT NULL DEFAULT 0,
    max_attempts    integer NOT NULL DEFAULT 10,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error      text,
    status          text NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending', 'running', 'done', 'failed')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS chain_jobs_due_idx
    ON chain_jobs (next_attempt_at)
    WHERE status = 'pending';

-- transfers — transferência exclusivamente pelo contrato da plataforma, com teto de
-- preço e royalty. origem, destino, preço, royalty apurado, tx.
CREATE TABLE IF NOT EXISTS transfers (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_id      uuid NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    from_wallet_id uuid,   -- referência solta a public.wallets
    to_wallet_id   uuid,   -- referência solta a public.wallets
    price_cents    bigint,
    royalty_cents  bigint,
    tx_hash        text,
    status         text NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending', 'confirmed', 'failed')),
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- royalty_entries — apuração do royalty por transferência.
CREATE TABLE IF NOT EXISTS royalty_entries (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    transfer_id  uuid NOT NULL REFERENCES transfers(id) ON DELETE CASCADE,
    amount_cents bigint NOT NULL,
    beneficiary  text,
    created_at   timestamptz NOT NULL DEFAULT now()
);
