-- Atestação e âncora: o eixo on-chain vira PROVA (âncora do resumo), não POSSE (token).
-- A exportação para carteira externa deixa de existir.
ALTER TABLE tickets DROP COLUMN IF EXISTS custody;
ALTER TABLE tickets DROP COLUMN IF EXISTS exported_at;

-- chain_jobs passa a carregar âncoras de atestado (reason=attestation). O agrupamento por
-- lote (amount) do mint deixa de existir — nenhum caminho materializa token.
ALTER TABLE chain_jobs DROP COLUMN IF EXISTS amount;
ALTER TABLE chain_jobs ADD COLUMN IF NOT EXISTS attestation_id uuid;
ALTER TABLE chain_jobs DROP CONSTRAINT IF EXISTS chain_jobs_kind_check;
ALTER TABLE chain_jobs ADD CONSTRAINT chain_jobs_kind_check
    CHECK (kind IN ('mint', 'transfer', 'burn', 'anchor'));
ALTER TABLE chain_jobs DROP CONSTRAINT IF EXISTS chain_jobs_reason_check;
ALTER TABLE chain_jobs ADD CONSTRAINT chain_jobs_reason_check
    CHECK (reason IN ('attestation'));

-- event_attestations: o registro canônico fechado e assinado do evento. Correção posterior
-- gera NOVA versão (supersedes_id) — nunca edição. Um atestado vigente por evento.
CREATE TABLE IF NOT EXISTS event_attestations (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id       uuid NOT NULL REFERENCES events(id),
    version        integer NOT NULL DEFAULT 1,
    supersedes_id  uuid REFERENCES event_attestations(id),
    payload        jsonb NOT NULL,
    digest         bytea NOT NULL,
    signature      bytea NOT NULL,
    closed_at      timestamptz NOT NULL DEFAULT now(),
    anchor_status  text NOT NULL DEFAULT 'none'
                     CHECK (anchor_status IN ('none', 'pending', 'anchored', 'failed')),
    anchor_tx_hash text,
    anchored_at    timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS event_attestations_current_key
    ON event_attestations (event_id)
    WHERE supersedes_id IS NULL;
