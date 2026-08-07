-- Ingresso, check-in e passe de temporada por produtor.

-- tickets — o ingresso. signature (Ed25519 sobre payload verificável offline) e
-- qr_nonce são preenchidos na emissão (Etapa 1.5). transferable_after NASCE
-- preenchido: imediato em Pix, 60 dias em cartão (garantido pela emissão).
-- chain_status desacopla a venda da rede: nasce 'none'/'pending' e o mint roda em
-- segundo plano (Etapa 1.8) — indisponibilidade de RPC nunca bloqueia a venda.
CREATE TABLE IF NOT EXISTS tickets (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id           uuid NOT NULL REFERENCES events(id),
    lot_id             uuid NOT NULL REFERENCES lots(id),
    order_id           uuid REFERENCES orders(id),
    seat_id            uuid REFERENCES seats(id),
    owner_subject_id   uuid,   -- referência solta a public.subjects
    owner_wallet_id    uuid,   -- referência solta a public.wallets
    half_price         boolean NOT NULL DEFAULT false,
    signature          bytea,
    qr_nonce           text,
    status             text NOT NULL DEFAULT 'active'
                         CHECK (status IN ('active', 'used', 'cancelled', 'transferred', 'burned')),
    transferable_after timestamptz NOT NULL,
    chain_status       text NOT NULL DEFAULT 'none'
                         CHECK (chain_status IN ('none', 'pending', 'minted', 'failed')),
    chain_token_id     text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);

-- INVARIANTE (garantido por índice, não pela aplicação): um assento não pode ter
-- dois ingressos ATIVOS. Ingresso de pista (seat_id NULL) não entra na regra.
CREATE UNIQUE INDEX IF NOT EXISTS tickets_active_seat_key
    ON tickets (event_id, seat_id)
    WHERE status = 'active' AND seat_id IS NOT NULL;

-- checkins — portão, operador, horário, reentrada. Um ingresso pode ter várias
-- entradas quando há reentrada; a prevenção de duplicidade offline + reconciliação
-- é da portaria (Etapa 1.6).
CREATE TABLE IF NOT EXISTS checkins (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_id  uuid NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    gate       text,
    operator   text,
    is_reentry boolean NOT NULL DEFAULT false,
    device_id  text,
    entered_at timestamptz NOT NULL DEFAULT now(),
    synced_at  timestamptz
);
CREATE INDEX IF NOT EXISTS checkins_ticket_idx ON checkins (ticket_id);

-- season_passes / season_pass_dates — passe de temporada (Etapa 2.3), com datas
-- destacáveis e repassáveis individualmente.
CREATE TABLE IF NOT EXISTS season_passes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL,
    price_cents bigint NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS season_pass_dates (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    season_pass_id uuid NOT NULL REFERENCES season_passes(id) ON DELETE CASCADE,
    event_id       uuid REFERENCES events(id),
    occurs_at      timestamptz,
    detachable     boolean NOT NULL DEFAULT true,
    transferable   boolean NOT NULL DEFAULT true
);
