-- Registro de chaves de atestação. A verificação pública resolve a chave PELO key_id do
-- atestado — nunca pela chave corrente — para que atestados antigos sigam verificáveis após
-- rotação. Chave aposentada permanece na tabela (retired_at preenchido manualmente).
CREATE TABLE IF NOT EXISTS attestation_keys (
    key_id     text PRIMARY KEY,
    public_key text NOT NULL,
    algorithm  text NOT NULL DEFAULT 'ed25519',
    created_at timestamptz NOT NULL DEFAULT now(),
    retired_at timestamptz
);
