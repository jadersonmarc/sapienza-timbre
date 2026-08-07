-- Portaria (Etapa 1.6). Prevenção de duplicidade garantida por SCHEMA, para valer
-- mesmo com scans offline reconciliados depois em vários dispositivos.

-- No máximo UMA admissão primária por ingresso. A reentrada é explícita (is_reentry):
-- essa não conflita. Na reconciliação do sync, a segunda admissão primária do mesmo
-- ingresso (dois portões admitindo a mesma pessoa) viola este índice → duplicata.
CREATE UNIQUE INDEX IF NOT EXISTS checkins_primary_admission_key
    ON checkins (ticket_id) WHERE NOT is_reentry;

-- client_uid: id gerado pelo dispositivo por scan; torna o sync idempotente (reenviar
-- o mesmo scan não duplica a linha).
ALTER TABLE checkins ADD COLUMN IF NOT EXISTS client_uid text;
CREATE UNIQUE INDEX IF NOT EXISTS checkins_client_uid_key
    ON checkins (client_uid) WHERE client_uid IS NOT NULL;
