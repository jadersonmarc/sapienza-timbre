-- Aparelhos da portaria.
--
-- A portaria valida OFFLINE com a chave pública embarcada. Isso tem um custo escondido: um
-- aparelho que ficou com a chave antiga recusa ingresso legítimo, e recusa com a mesma cara
-- de um ingresso falso. Sem registro de qual chave cada aparelho carrega e de quando ele
-- falou com o servidor pela última vez, esse problema só aparece na fila da porta.
--
-- Uma linha por aparelho, atualizada a cada sincronização. O aparelho que nunca sincronizou
-- não está aqui — e é exatamente por isso que a ausência dele na lista também é informação.
CREATE TABLE IF NOT EXISTS gate_devices (
    device_id       text PRIMARY KEY,
    key_fingerprint text,               -- impressão da chave pública embarcada no aparelho
    last_gate       text,
    last_operator   text,
    checkins_synced bigint NOT NULL DEFAULT 0,
    first_seen_at   timestamptz NOT NULL DEFAULT now(),
    last_sync_at    timestamptz NOT NULL DEFAULT now()
);
