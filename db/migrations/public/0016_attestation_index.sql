-- Índice público dos atestados: resolve attestation_id -> produtor, para a verificação
-- pública sem tenant no path.
CREATE TABLE IF NOT EXISTS attestation_index (
    attestation_id uuid PRIMARY KEY,
    producer_id    uuid NOT NULL REFERENCES producers(id) ON DELETE CASCADE,
    event_id       uuid NOT NULL
);
CREATE INDEX IF NOT EXISTS attestation_index_event_idx ON attestation_index (event_id);
