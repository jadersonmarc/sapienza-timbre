-- Índice público das sessões de checkout: resolve session_id -> produtor, para as rotas
-- públicas da sessão (retomada antes do acesso) sem tenant no path.
CREATE TABLE IF NOT EXISTS checkout_session_index (
    session_id uuid PRIMARY KEY,
    producer_id uuid NOT NULL REFERENCES producers(id) ON DELETE CASCADE,
    event_id    uuid NOT NULL
);
