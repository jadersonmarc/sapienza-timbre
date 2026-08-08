-- Metadados públicos do token (Etapa 1.9), padrão ERC-1155, renderizáveis por carteiras
-- externas. SEM DADO PESSOAL: só evento, data, local, setor/fileira/assento, lote e valor
-- de face. Em public para o token ser resolvível por ticket_id sem varrer schemas.
CREATE TABLE IF NOT EXISTS token_metadata (
    ticket_id   uuid PRIMARY KEY,
    producer_id uuid NOT NULL REFERENCES producers(id) ON DELETE CASCADE,
    name        text NOT NULL,
    description text,
    attributes  jsonb NOT NULL DEFAULT '[]',
    updated_at  timestamptz NOT NULL DEFAULT now()
);
