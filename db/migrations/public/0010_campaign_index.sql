-- Índice público de campanhas (Etapa 2.8): resolve campaign_id → produtor/evento para o
-- registro de clique vindo do link parametrizado (público), sem varrer schemas.
CREATE TABLE IF NOT EXISTS campaign_index (
    campaign_id uuid PRIMARY KEY,
    producer_id uuid NOT NULL REFERENCES producers(id) ON DELETE CASCADE,
    event_id    uuid NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
