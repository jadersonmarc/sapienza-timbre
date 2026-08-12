-- Audiência (Fase 3). As tabelas já existem (0002); aqui só índices de apoio.
-- Consulta do consentimento vigente por (sujeito, segmento).
CREATE INDEX IF NOT EXISTS consents_subject_segment_idx ON consents (subject_id, segment_id, granted_at DESC);
-- Uma entrega por (campanha, sujeito) — o alcance não duplica.
CREATE UNIQUE INDEX IF NOT EXISTS campaign_deliveries_unique_key ON campaign_deliveries (campaign_id, subject_id);
