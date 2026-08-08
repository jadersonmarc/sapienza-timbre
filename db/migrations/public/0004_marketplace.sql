-- Suporte ao mercado secundário no mundo público (Etapa 2.2).

-- Distingue no índice de pagamento a compra primária (order) da revenda (resale), para
-- o webhook global rotear ao fluxo certo.
ALTER TABLE payment_index ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'order';

-- Índice público de anúncios: resolve listing → produtor (e ingresso) para a compra
-- pública e a procedência, sem varrer schemas.
CREATE TABLE IF NOT EXISTS listing_index (
    listing_id  uuid PRIMARY KEY,
    producer_id uuid NOT NULL REFERENCES producers(id) ON DELETE CASCADE,
    ticket_id   uuid NOT NULL,
    price_cents bigint NOT NULL,
    status      text NOT NULL DEFAULT 'active',
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS listing_index_producer_idx ON listing_index (producer_id);
