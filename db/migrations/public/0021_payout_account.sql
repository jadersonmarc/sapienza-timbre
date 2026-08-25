-- Dados de repasse do produtor. Enquanto a divisão automática na venda (split) não estiver
-- em uso, a plataforma recebe o total e transfere a parte do produtor depois — e para isso
-- basta uma chave Pix, não a abertura de conta no gateway.
--
-- A carteira do split (asaas_wallet_id) CONTINUA existindo e tem precedência quando
-- preenchida: o dia em que a subconta for viável, nada aqui precisa ser desfeito.
ALTER TABLE producers
    ADD COLUMN IF NOT EXISTS payout_pix_key       text,
    ADD COLUMN IF NOT EXISTS payout_pix_key_type  text
        CHECK (payout_pix_key_type IS NULL OR payout_pix_key_type IN ('cpf','cnpj','email','phone','random')),
    ADD COLUMN IF NOT EXISTS payout_holder_name   text,
    ADD COLUMN IF NOT EXISTS payout_holder_tax_id text;

COMMENT ON COLUMN producers.payout_pix_key IS
    'Chave Pix de repasse. O titular precisa bater com o documento informado — transferir para chave de terceiro é problema fiscal do produtor e nosso.';
