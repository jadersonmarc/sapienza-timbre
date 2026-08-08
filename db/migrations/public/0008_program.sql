-- Programa de produtores (Etapa 2.7, CORRIGIDA). Valores DEFINIDOS: taxa do Timbre 15%;
-- níveis Iniciante 10% / Pro 15% / Sênior 20%. Estes percentuais são versionados por
-- data de vigência. A relação entre o % do nível e a taxa é PROVISÓRIA (ver internal/
-- program) — pendente de definição comercial.

CREATE TABLE IF NOT EXISTS platform_fee_rules (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    effective_from     timestamptz NOT NULL DEFAULT now(),
    fee_pct            numeric(5,2) NOT NULL,   -- taxa do Timbre sobre o valor do ingresso
    tier_iniciante_pct numeric(5,2) NOT NULL,
    tier_pro_pct       numeric(5,2) NOT NULL,
    tier_senior_pct    numeric(5,2) NOT NULL
);

-- Seed com os valores DEFINIDOS (idempotente).
INSERT INTO platform_fee_rules (fee_pct, tier_iniciante_pct, tier_pro_pct, tier_senior_pct)
SELECT 15, 10, 15, 20
 WHERE NOT EXISTS (SELECT 1 FROM platform_fee_rules);

-- Histórico de vigências do nível do produtor. A apuração usa o nível vigente NA DATA DA
-- VENDA; a transição nunca é retroativa sobre eventos já apurados.
CREATE TABLE IF NOT EXISTS producer_tier_history (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    producer_id    uuid NOT NULL REFERENCES producers(id) ON DELETE CASCADE,
    tier           text NOT NULL CHECK (tier IN ('iniciante','pro','senior')),
    effective_from timestamptz NOT NULL DEFAULT now(),
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS producer_tier_history_idx ON producer_tier_history (producer_id, effective_from DESC);

-- Originação: produtor indicado por um originador. participation_pct e effective_until
-- são PROVISÓRIOS (sem valor definido — default 0/nulo até definição comercial).
CREATE TABLE IF NOT EXISTS originations (
    producer_id           uuid PRIMARY KEY REFERENCES producers(id) ON DELETE CASCADE,
    originator_producer_id uuid NOT NULL REFERENCES producers(id) ON DELETE CASCADE,
    participation_pct     numeric(5,2) NOT NULL DEFAULT 0,   -- PROVISÓRIO
    effective_until       timestamptz,                        -- PROVISÓRIO (nulo = sem prazo)
    created_at            timestamptz NOT NULL DEFAULT now()
);

-- Apuração da participação do originador, evento por evento / ordem por ordem.
CREATE TABLE IF NOT EXISTS origination_entries (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    originator_producer_id uuid NOT NULL REFERENCES producers(id) ON DELETE CASCADE,
    producer_id            uuid NOT NULL REFERENCES producers(id) ON DELETE CASCADE,
    event_id               uuid,
    order_id               uuid,
    amount_cents           bigint NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS origination_entries_originator_idx ON origination_entries (originator_producer_id);
