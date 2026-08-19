-- Painel administrativo global (substitui o /dash embutido): operadores de
-- plataforma com papéis (admin/super_admin), catálogo global de artistas, fila de
-- moderação e trilha de auditoria. Tudo em `public` — é a camada compartilhada do
-- Timbre. Não é tenant: o admin enxerga a plataforma inteira.

-- admins — operador da plataforma. Auth JWT própria (escopo "admin"), separada da do
-- colaborador (escopo do produtor) e da do comprador (escopo "buyer"). super_admin
-- tem acesso total; admin opera sem gerir outros admins.
CREATE TABLE IF NOT EXISTS admins (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email           text NOT NULL UNIQUE,
    password_hash   text NOT NULL,
    role            text NOT NULL DEFAULT 'admin'
                      CHECK (role IN ('admin', 'super_admin')),
    -- incrementado na troca de senha para invalidar JWTs antigos (mesmo padrão de
    -- collaborators.session_version).
    session_version integer NOT NULL DEFAULT 1,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- artists — catálogo global de artistas, CROSS-TENANT. O evento vive no schema do
-- produtor; o vínculo evento->artista fica aqui por referência solta (sem FK
-- cross-schema), mesmo padrão de attendance_records.
CREATE TABLE IF NOT EXISTS artists (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL,
    slug        text NOT NULL UNIQUE,
    bio         text,
    image_url   text,
    category    text,
    status      text NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active', 'suspended')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- event_artists — vínculo solto evento <-> artista. producer_id/event_id apontam para
-- o schema do produtor sem FK (o schema é o isolamento); artist_id é FK real para o
-- catálogo global acima.
CREATE TABLE IF NOT EXISTS event_artists (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    producer_id uuid,
    event_id    uuid,
    artist_id   uuid NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    headline    boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS event_artists_event_idx ON event_artists (event_id);
CREATE INDEX IF NOT EXISTS event_artists_artist_idx ON event_artists (artist_id);

-- moderation_flags — denúncias (evento, artista, produtor, comprador). Fila consumida
-- pelo /admin. Moderação é REATIVA: nada nasce bloqueado por aprovação manual.
CREATE TABLE IF NOT EXISTS moderation_flags (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    target_type text NOT NULL CHECK (target_type IN ('event', 'artist', 'producer', 'buyer')),
    target_id   uuid NOT NULL,
    reason      text NOT NULL,
    status      text NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending', 'resolved', 'dismissed')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    resolved_by uuid REFERENCES admins(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS moderation_flags_status_idx ON moderation_flags (status, created_at);

-- audit_log — trilha de ações administrativas (quem fez o quê, quando, sobre o quê).
-- Append-only: nunca se edita/remove linha.
CREATE TABLE IF NOT EXISTS audit_log (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    admin_id    uuid REFERENCES admins(id) ON DELETE SET NULL,
    action      text NOT NULL,
    entity_type text,
    entity_id   uuid,
    details     jsonb NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_log_created_idx ON audit_log (created_at DESC);
