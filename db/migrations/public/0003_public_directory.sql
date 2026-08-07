-- Índices públicos que ligam o mundo público (comprador sem conta, webhook global do
-- Asaas) ao schema do produtor certo. Sem eles, um event_id ou um asaas_ref não diz em
-- qual tenant_<id> procurar.

-- event_directory: qual produtor é dono de um evento publicado. Alimentado no publish.
-- Também é a base da descoberta pública (Camada 3) mais adiante.
CREATE TABLE IF NOT EXISTS event_directory (
    event_id     uuid PRIMARY KEY,
    producer_id  uuid NOT NULL REFERENCES producers(id) ON DELETE CASCADE,
    title        text NOT NULL,
    category     text,
    starts_at    timestamptz,
    published_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS event_directory_producer_idx ON event_directory (producer_id);

-- payment_index: qual produtor/ordem corresponde a uma cobrança do Asaas. O webhook é
-- global (um só URL); daqui resolvemos o tenant para processar.
CREATE TABLE IF NOT EXISTS payment_index (
    asaas_ref   text PRIMARY KEY,
    producer_id uuid NOT NULL REFERENCES producers(id) ON DELETE CASCADE,
    order_id    uuid NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- Carteira Asaas do produtor, para o split no ato da venda (nullable até conectar).
ALTER TABLE producers ADD COLUMN IF NOT EXISTS asaas_wallet_id text;
