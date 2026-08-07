-- Camada de identidade e audiência — CROSS-PRODUTOR. A trajetória de uma pessoa
-- cruza vários produtores, então estas tabelas vivem em `public` (compartilhado),
-- não nos schemas por-produtor. Nenhuma referencia dado pessoal em payload de rede;
-- o vínculo pessoa <-> carteira é apagável a pedido. As colunas producer_id/
-- event_id/ticket_id/checkin_id são referências SOLTAS (sem FK cross-schema para
-- tenant_<id>), o que também facilita apagar a pedido.
--
-- As tabelas das Fases 2 e 3 já entram aqui para nunca reescrever schema depois.

-- subjects — a pessoa. Pode não ter conta (o comprador de ingresso sequer cadastra).
CREATE TABLE IF NOT EXISTS subjects (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    display_name text,
    email        text,
    cpf          text,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- wallets — vínculo pessoa <-> endereço. Fica FORA de qualquer payload de rede;
-- apagável a pedido (ON DELETE CASCADE do subject).
CREATE TABLE IF NOT EXISTS wallets (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_id uuid NOT NULL REFERENCES subjects(id) ON DELETE CASCADE,
    address    text NOT NULL UNIQUE,
    chain      text NOT NULL DEFAULT 'base',
    -- carteira invisível por MPC criada na autenticação social.
    custody    text NOT NULL DEFAULT 'mpc',
    created_at timestamptz NOT NULL DEFAULT now()
);

-- attendance_records — registro de presença. INTRANSFERÍVEL por ausência de
-- mecanismo: NÃO existe coluna de transferência aqui, de propósito.
CREATE TABLE IF NOT EXISTS attendance_records (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_id  uuid REFERENCES subjects(id) ON DELETE SET NULL,
    producer_id uuid,   -- referência solta (produtor em public.producers)
    event_id    uuid,   -- referência solta (evento no schema do produtor)
    ticket_id   uuid,   -- referência solta
    checkin_id  uuid,   -- referência solta
    gate        text,
    occurred_at timestamptz NOT NULL DEFAULT now()
);

-- trails / trail_progress — trilhas e passaporte cultural (Fase 2.5).
CREATE TABLE IF NOT EXISTS trails (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL,
    description text,
    sponsor     text,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS trail_progress (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    trail_id     uuid NOT NULL REFERENCES trails(id) ON DELETE CASCADE,
    subject_id   uuid NOT NULL REFERENCES subjects(id) ON DELETE CASCADE,
    progress     integer NOT NULL DEFAULT 0,
    completed_at timestamptz,
    UNIQUE (trail_id, subject_id)
);

-- reviews — avaliação de evento restrita a quem fez check-in: checkin_id é NOT NULL.
CREATE TABLE IF NOT EXISTS reviews (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_id  uuid REFERENCES subjects(id) ON DELETE SET NULL,
    producer_id uuid,
    event_id    uuid,
    checkin_id  uuid NOT NULL,   -- vínculo obrigatório a um check-in
    rating      integer NOT NULL CHECK (rating BETWEEN 1 AND 5),
    body        text,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- producer_reputation — agregados verificáveis por produtor (Fase 2.6).
CREATE TABLE IF NOT EXISTS producer_reputation (
    producer_id       uuid PRIMARY KEY,
    events_delivered  integer NOT NULL DEFAULT 0,
    cancellation_rate numeric(5,2) NOT NULL DEFAULT 0,
    refund_rate       numeric(5,2) NOT NULL DEFAULT 0,
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- audience_segments / segment_memberships — segmentação por presença (Fase 3.1).
CREATE TABLE IF NOT EXISTS audience_segments (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    definition jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS segment_memberships (
    segment_id uuid NOT NULL REFERENCES audience_segments(id) ON DELETE CASCADE,
    subject_id uuid NOT NULL REFERENCES subjects(id) ON DELETE CASCADE,
    added_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (segment_id, subject_id)
);

-- consents — por segmento, granular, revogável, com histórico (linhas append).
CREATE TABLE IF NOT EXISTS consents (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_id uuid NOT NULL REFERENCES subjects(id) ON DELETE CASCADE,
    segment_id uuid NOT NULL REFERENCES audience_segments(id) ON DELETE CASCADE,
    granted    boolean NOT NULL,
    granted_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);

-- consent_rewards — recompensa de quem autoriza (crédito, desconto ou ingresso).
CREATE TABLE IF NOT EXISTS consent_rewards (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    consent_id uuid NOT NULL REFERENCES consents(id) ON DELETE CASCADE,
    kind       text NOT NULL CHECK (kind IN ('credito', 'desconto', 'ingresso')),
    amount     numeric(12,2),
    issued_at  timestamptz NOT NULL DEFAULT now()
);

-- sponsor_campaigns / campaign_deliveries — comercialização de alcance (Fase 3.4).
-- Sem exposição de identidade individual ao patrocinador; o painel agrega.
CREATE TABLE IF NOT EXISTS sponsor_campaigns (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    sponsor    text NOT NULL,
    segment_id uuid REFERENCES audience_segments(id) ON DELETE SET NULL,
    budget     numeric(12,2),
    status     text NOT NULL DEFAULT 'draft'
                 CHECK (status IN ('draft', 'active', 'paused', 'done')),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS campaign_deliveries (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id  uuid NOT NULL REFERENCES sponsor_campaigns(id) ON DELETE CASCADE,
    subject_id   uuid REFERENCES subjects(id) ON DELETE SET NULL,
    delivered_at timestamptz NOT NULL DEFAULT now(),
    metric       jsonb NOT NULL DEFAULT '{}'
);
