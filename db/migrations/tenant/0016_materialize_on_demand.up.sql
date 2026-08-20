-- Emissão on-chain sob demanda: o ingresso nasce 'not_materialized' e só vira 'pending'
-- quando a posse precisa se mover na cadeia (revenda, exportação, colecionável, backfill).
-- 'not_materialized' é normal e permanente; 'pending' é transitório (mint em fila).

ALTER TABLE tickets DROP CONSTRAINT IF EXISTS tickets_chain_status_check;
ALTER TABLE tickets ADD CONSTRAINT tickets_chain_status_check
    CHECK (chain_status IN ('not_materialized', 'pending', 'minted', 'failed'));
ALTER TABLE tickets ALTER COLUMN chain_status SET DEFAULT 'not_materialized';
-- Backfill: linhas antigas em 'none' (default anterior) viram 'not_materialized'.
UPDATE tickets SET chain_status = 'not_materialized' WHERE chain_status = 'none';

-- chain_jobs: razão da materialização, custo de gás medido e hash da transação. amount
-- permite agrupar ingressos de pista do mesmo lote numa única transação (ERC-1155 por lote).
ALTER TABLE chain_jobs ADD COLUMN IF NOT EXISTS reason text
    CHECK (reason IN ('export', 'resale_listing', 'collectible', 'bulk_producer', 'backfill'));
ALTER TABLE chain_jobs ADD COLUMN IF NOT EXISTS gas_cost_wei numeric;
ALTER TABLE chain_jobs ADD COLUMN IF NOT EXISTS tx_hash text;
ALTER TABLE chain_jobs ADD COLUMN IF NOT EXISTS amount integer NOT NULL DEFAULT 1;
