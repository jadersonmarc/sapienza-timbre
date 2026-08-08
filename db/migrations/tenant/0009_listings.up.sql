-- Mercado secundário oficial (Etapa 2.2): o anúncio de revenda de um ingresso dentro
-- da plataforma. A troca de titularidade em si é a transferência restrita (2.1); aqui é
-- a oferta e o vínculo com o pagamento do comprador.
CREATE TABLE IF NOT EXISTS listings (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_id       uuid NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    price_cents     bigint NOT NULL,
    seller_wallet_id uuid,          -- dono no momento do anúncio (referência solta)
    buyer_wallet_id  uuid,          -- carteira do comprador (preenchida na compra)
    buyer_order_id   uuid REFERENCES orders(id),
    status          text NOT NULL DEFAULT 'active'
                      CHECK (status IN ('active', 'reserved', 'sold', 'cancelled')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    sold_at         timestamptz
);

-- No máximo um anúncio vivo (ativo ou reservado) por ingresso.
CREATE UNIQUE INDEX IF NOT EXISTS listings_live_ticket_key
    ON listings (ticket_id) WHERE status IN ('active', 'reserved');
