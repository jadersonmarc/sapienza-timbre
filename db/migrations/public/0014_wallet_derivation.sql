-- Carteiras por participante: derivação determinística de endereço (sem fornecedor de
-- carteira/MPC) e endereços importados. A semente vive no cofre (CHAIN_HD_SEED_REF aponta
-- para ela — nunca a semente em si). derivation_index NUNCA é reaproveitado: uma sequência
-- global aloca o próximo, mesmo após apagar a conta.

ALTER TABLE wallets ADD COLUMN IF NOT EXISTS derivation_index bigint;
ALTER TABLE wallets ADD COLUMN IF NOT EXISTS origin text NOT NULL DEFAULT 'derived'
    CHECK (origin IN ('derived', 'imported'));
CREATE UNIQUE INDEX IF NOT EXISTS wallets_derivation_index_key ON wallets (derivation_index);
CREATE SEQUENCE IF NOT EXISTS wallet_derivation_seq;

-- ticket_directory ganha o estado de materialização on-chain, para "meus ingressos"
-- distinguir o normal/permanente (not_materialized) do transitório (pending).
ALTER TABLE ticket_directory ADD COLUMN IF NOT EXISTS chain_status text NOT NULL DEFAULT 'not_materialized';
