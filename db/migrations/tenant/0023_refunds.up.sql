-- Estorno originado no Timbre: a operação, os ingressos que ela alcança e a reversão do
-- repasse. Até aqui o estorno só existia como reação a webhook do gateway, e queimava a
-- ordem inteira.

-- ── ordem parcialmente estornada ──────────────────────────────────────────────
-- Estado novo: sobrou ingresso válido no pedido. Sem ele, estornar um de quatro obrigaria
-- a marcar o pedido inteiro como estornado, e os três que sobraram sumiriam do histórico
-- de quem os comprou.
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_status_check
    CHECK (status IN ('pending', 'paid', 'cancelled', 'refunded', 'partially_refunded'));

-- ── a operação de estorno ─────────────────────────────────────────────────────
-- Separada da AUTORIZAÇÃO (quem pediu, qual trilha, quem decidiu — vem depois): aqui está
-- só o que foi estornado, por quanto, e em que pé está no gateway.
--
-- O estorno é feito em duas fases porque a chamada ao gateway não pode viver dentro da
-- transação: se o dinheiro volta e a transação faz rollback, o comprador foi estornado e o
-- ingresso continua válido, sem registro nenhum. Esta linha é o registro que sobrevive
-- entre a intenção e o efeito.
CREATE TABLE IF NOT EXISTS refunds (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id          uuid NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    payment_id        uuid REFERENCES payments(id),
    scope             text NOT NULL CHECK (scope IN ('total', 'partial')),
    -- Os ingressos alcançados. Guardados na linha porque depois do estorno eles estão
    -- queimados e não dá mais para deduzir quais foram por consulta.
    ticket_ids        uuid[] NOT NULL,
    face_cents        bigint NOT NULL DEFAULT 0,
    convenience_cents bigint NOT NULL DEFAULT 0,
    -- A tarifa do gateway não volta. Fica registrada para saber o custo real do estorno.
    gateway_fee_cents bigint NOT NULL DEFAULT 0,
    total_cents       bigint NOT NULL DEFAULT 0,
    status            text NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'sent', 'confirmed', 'failed')),
    gateway_refund_id text,
    error             text,
    initiated_by      text NOT NULL CHECK (initiated_by IN ('webhook', 'producer', 'admin')),
    reason            text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS refunds_order_idx  ON refunds (order_id);
CREATE INDEX IF NOT EXISTS refunds_status_idx ON refunds (status);
CREATE UNIQUE INDEX IF NOT EXISTS refunds_gateway_key
    ON refunds (gateway_refund_id) WHERE gateway_refund_id IS NOT NULL;

-- INVARIANTE (por índice, não pela aplicação): um ingresso não entra em dois estornos
-- vivos. Mesma técnica do seat_occupancy — a unicidade parcial resolve o duplo clique e o
-- retry concorrente sem depender de quem chama.
CREATE TABLE IF NOT EXISTS refund_tickets (
    refund_id  uuid NOT NULL REFERENCES refunds(id) ON DELETE CASCADE,
    ticket_id  uuid NOT NULL REFERENCES tickets(id),
    face_cents bigint NOT NULL,
    -- Redundante com refunds.status, e de propósito: é sobre ele que o índice parcial
    -- abaixo decide o que é "estorno vivo".
    dead       boolean NOT NULL DEFAULT false,
    PRIMARY KEY (refund_id, ticket_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS refund_tickets_live_key
    ON refund_tickets (ticket_id) WHERE NOT dead;

-- ── reversão do repasse ───────────────────────────────────────────────────────
-- split_transfers tem order_id como PK: uma linha por pedido. Estorno PARCIAL não pode
-- sobrescrever o status dela, senão um ingresso de quatro derruba o repasse inteiro. A
-- reversão é registrada por ingresso, e split_transfers.split_status só vai a REFUNDED
-- quando não sobra face no pedido.
CREATE TABLE IF NOT EXISTS split_refunds (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    refund_id   uuid NOT NULL REFERENCES refunds(id) ON DELETE CASCADE,
    order_id    uuid NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    ticket_id   uuid NOT NULL REFERENCES tickets(id),
    face_cents  bigint NOT NULL,
    -- De onde saiu o valor devolvido ao comprador:
    --   not_settled      — o split ainda não liquidou; o estorno da cobrança o cancela junto
    --   platform_balance — venda centralizada: o dinheiro ainda estava com a plataforma
    --                      (inclui o que a retenção de 5%/60d segurava)
    --   producer         — puxado da subconta do produtor pelo estorno da cobrança
    --   platform_covered — a subconta não cobriu; a plataforma cobriu e o produtor ficou
    --                      devendo, e a dívida sai dos próximos repasses
    source      text NOT NULL CHECK (source IN ('not_settled', 'platform_balance', 'producer', 'platform_covered')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (refund_id, ticket_id)
);

CREATE INDEX IF NOT EXISTS split_refunds_order_idx ON split_refunds (order_id);
