-- Remove o eixo de posse/MPC. A derivação determinística de endereço deixa de existir:
-- o eixo on-chain agora é prova (âncora do atestado), não posse (token/carteira).
ALTER TABLE wallets DROP COLUMN IF EXISTS derivation_index;
ALTER TABLE wallets DROP COLUMN IF EXISTS origin;
DROP SEQUENCE IF EXISTS wallet_derivation_seq;
