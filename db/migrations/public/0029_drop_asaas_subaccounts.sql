-- Produtor deixa de ter conta no gateway.
--
-- A subconta existia por um motivo só: ser destinatária do split de cada venda. Sem split,
-- ela é KYC de terceiro que a plataforma pedia, guardava e mantinha — responsabilidade sem
-- contrapartida. E o teto regulatório de dez subcontas com relógio de 60 dias, que era
-- bloqueador de operação, some junto.
DROP TABLE IF EXISTS producer_asaas_documents;
DROP TABLE IF EXISTS producer_asaas_accounts;

-- A carteira espelhada no produtor era de onde o checkout lia o destinatário do split.
ALTER TABLE producers DROP COLUMN IF EXISTS asaas_wallet_id;
