-- Idempotência de webhook pelo ID DO EVENTO, não pelo id da cobrança: uma mesma cobrança
-- gera vários eventos (confirmação, split liquidado, estorno), e deduplicar por cobrança
-- descartaria eventos legítimos.
CREATE TABLE IF NOT EXISTS webhook_events (
    event_id     text PRIMARY KEY,
    event_type   text,
    received_at  timestamptz NOT NULL DEFAULT now()
);
