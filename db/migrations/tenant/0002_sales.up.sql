-- Venda por produtor. buyer_subject_id/*_wallet_id apontam para public.subjects/
-- wallets por referência SOLTA (sem FK): o comprador é cross-produtor e apagável.

-- orders — o pedido.
CREATE TABLE IF NOT EXISTS orders (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id         uuid NOT NULL REFERENCES events(id),
    buyer_subject_id uuid,   -- referência solta a public.subjects
    buyer_email      text,
    buyer_cpf        text,
    coupon_id        uuid,   -- preenchido no redeem (FK lógica a coupons)
    total_cents      bigint NOT NULL DEFAULT 0,
    status           text NOT NULL DEFAULT 'pending'
                       CHECK (status IN ('pending', 'paid', 'cancelled', 'refunded')),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- order_items — itens do pedido (lote/tipo, quantidade e, se houver mapa, assento).
CREATE TABLE IF NOT EXISTS order_items (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id         uuid NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    lot_id           uuid NOT NULL REFERENCES lots(id),
    sector_id        uuid REFERENCES sectors(id),
    seat_id          uuid REFERENCES seats(id),
    quantity         integer NOT NULL DEFAULT 1,
    unit_price_cents bigint NOT NULL,
    half_price       boolean NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now()
);

-- holds — reserva com expires_at. O MOTOR de reserva (Hold/Release/Confirm +
-- FOR UPDATE SKIP LOCKED + varredura de expiração + TTL) é da Etapa 1.3; aqui já
-- deixamos o invariante intra-tabela: um assento não tem dois holds VIVOS.
CREATE TABLE IF NOT EXISTS holds (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id   uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    seat_id    uuid REFERENCES seats(id) ON DELETE CASCADE,
    order_id   uuid REFERENCES orders(id) ON DELETE SET NULL,
    status     text NOT NULL DEFAULT 'held'
                 CHECK (status IN ('held', 'released', 'confirmed', 'expired')),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Um assento não pode ter dois holds vivos. Expiração é feita pela varredura que
-- muda status -> 'expired' (now() não entra em predicado de índice).
CREATE UNIQUE INDEX IF NOT EXISTS holds_live_seat_key
    ON holds (event_id, seat_id)
    WHERE status = 'held' AND seat_id IS NOT NULL;

-- coupons — limite de uso e validade.
CREATE TABLE IF NOT EXISTS coupons (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id       uuid REFERENCES events(id) ON DELETE CASCADE,
    code           text NOT NULL,
    discount_pct   numeric(5,2),
    discount_cents bigint,
    max_uses       integer,
    uses           integer NOT NULL DEFAULT 0,
    valid_from     timestamptz,
    valid_until    timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (event_id, code)
);

CREATE TABLE IF NOT EXISTS coupon_redemptions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    coupon_id   uuid NOT NULL REFERENCES coupons(id) ON DELETE CASCADE,
    order_id    uuid NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    redeemed_at timestamptz NOT NULL DEFAULT now()
);

-- guest_list_entries — cortesias/convidados, com CPF e assento específico opcional.
CREATE TABLE IF NOT EXISTS guest_list_entries (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id   uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    name       text NOT NULL,
    cpf        text,
    lot_id     uuid REFERENCES lots(id),
    seat_id    uuid REFERENCES seats(id),
    status     text NOT NULL DEFAULT 'invited'
                 CHECK (status IN ('invited', 'issued', 'checked_in')),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- payments — referência Asaas, método, status, settled_at. split = divisão no ato.
CREATE TABLE IF NOT EXISTS payments (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id     uuid NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    method       text NOT NULL CHECK (method IN ('pix', 'credit_card')),
    asaas_ref    text,
    installments integer NOT NULL DEFAULT 1,
    amount_cents bigint NOT NULL,
    status       text NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'confirmed', 'failed', 'refunded')),
    split        jsonb NOT NULL DEFAULT '{}',
    settled_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
