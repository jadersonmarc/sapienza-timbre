-- Repasse ao produtor por pedido: o que foi combinado, como foi calculado e o que o gateway
-- fez com ele. Uma linha por pedido; o split é sempre um só (o produtor).
--
-- fee_snapshot guarda a tabela de tarifas usada NO MOMENTO do cálculo. Sem ela, um preço
-- de semanas atrás é indefensável: a tabela pode ter mudado, e a diferença entre "erramos"
-- e "a tarifa mudou" é exatamente esse registro.
CREATE TABLE IF NOT EXISTS split_transfers (
    order_id              uuid PRIMARY KEY REFERENCES orders(id) ON DELETE CASCADE,
    event_id              uuid NOT NULL REFERENCES events(id),
    producer_id           uuid NOT NULL,
    face_cents            bigint NOT NULL,
    convenience_cents     bigint NOT NULL,
    platform_margin_cents bigint NOT NULL,
    payment_method        text   NOT NULL,
    installments          int    NOT NULL DEFAULT 1,
    asaas_payment_id      text,
    -- Chega pelo webhook de liquidação; nulo na criação.
    asaas_split_id        text,
    split_status          text NOT NULL DEFAULT 'PENDING'
        CHECK (split_status IN ('PENDING','AWAITING_CREDIT','CANCELLED','DONE','REFUSED','REFUNDED','BLOCKED')),
    refusal_reason        text,
    fee_snapshot          jsonb,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS split_transfers_payment_idx ON split_transfers (asaas_payment_id);
CREATE INDEX IF NOT EXISTS split_transfers_status_idx  ON split_transfers (split_status);

-- Rateio do line-up: quanto de cada venda cabe a cada artista, por percentual. É
-- INFORMATIVO — alimenta o painel do produtor e não movimenta dinheiro. Artista não é
-- recebedor no gateway; quem paga o artista é o produtor.
CREATE TABLE IF NOT EXISTS lineup_shares (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id       uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    artist_id      uuid,
    artist_name    text NOT NULL,
    share_pct      numeric(5,2) NOT NULL CHECK (share_pct >= 0 AND share_pct <= 100),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS lineup_shares_event_idx ON lineup_shares (event_id);
