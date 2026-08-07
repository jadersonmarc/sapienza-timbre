-- Control plane do Timbre: registro de produtores (o tenant) + colaboradores e
-- permissões granulares. Vive em `public` (banco próprio do Timbre). Cada produtor
-- ganha, além da linha aqui, um schema tenant_<id> com a operação (catálogo, venda,
-- ingresso, ...) provisionado pelo kit/tenancy.

-- producers — o tenant. id é o tenant_id usado em tenancy.SchemaName.
CREATE TABLE IF NOT EXISTS producers (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL,
    -- senioridade medida em ingressos vendidos em 12 meses (cálculo na 2.7).
    tier          text NOT NULL DEFAULT 'iniciante'
                    CHECK (tier IN ('iniciante', 'pro', 'senior')),
    -- percentual retido pela plataforma sobre a taxa (varia por tier/originação).
    retention_pct numeric(5,2) NOT NULL DEFAULT 0,
    status        text NOT NULL DEFAULT 'active'
                    CHECK (status IN ('pending', 'active', 'suspended')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- collaborators — quem opera em nome do produtor. Auth é nativa do Timbre.
CREATE TABLE IF NOT EXISTS collaborators (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    producer_id     uuid NOT NULL REFERENCES producers(id) ON DELETE CASCADE,
    email           text NOT NULL,
    password_hash   text NOT NULL,
    -- owner tem todas as permissões implicitamente e administra colaboradores.
    is_owner        boolean NOT NULL DEFAULT false,
    -- incrementado na troca de senha para invalidar JWTs antigos.
    session_version integer NOT NULL DEFAULT 1,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- e-mail único por produtor (normalizado em minúsculas pela aplicação).
CREATE UNIQUE INDEX IF NOT EXISTS collaborators_producer_email_key
    ON collaborators (producer_id, email);

-- collaborator_permissions — granularidade exigida na 1.1: check-in, financeiro,
-- relatórios, atendimento. owner não precisa de linhas aqui (passa sempre).
CREATE TABLE IF NOT EXISTS collaborator_permissions (
    collaborator_id uuid NOT NULL REFERENCES collaborators(id) ON DELETE CASCADE,
    permission      text NOT NULL
                      CHECK (permission IN ('checkin', 'financeiro', 'relatorios', 'atendimento')),
    PRIMARY KEY (collaborator_id, permission)
);
