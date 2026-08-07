-- Catálogo por produtor. Aplicado a cada schema tenant_<id> via
-- kit/tenancy.MigrationRunner (search_path = tenant_<id>, public). Sem coluna de
-- produtor: o schema É o produtor (isolamento por schema, convenção do kit/Margot).

-- events — o evento. has_seat_map liga o editor de mapa (opcional por evento).
CREATE TABLE IF NOT EXISTS events (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    title               text NOT NULL,
    description         text,
    category            text NOT NULL
                          CHECK (category IN ('shows', 'teatro', 'festas', 'esportes',
                                              'congressos', 'cursos', 'workshops', 'gastronomia')),
    cover_url           text,
    starts_at           timestamptz,
    ends_at             timestamptz,
    address             text,
    lat                 double precision,
    lng                 double precision,
    capacity            integer,
    age_rating          text,
    cancellation_policy text,
    terms               text,
    has_seat_map        boolean NOT NULL DEFAULT false,
    status              text NOT NULL DEFAULT 'draft'
                          CHECK (status IN ('draft', 'published', 'suspended', 'finished', 'cancelled')),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- lots — lotes ilimitados e configuráveis. Virada automática ao esgotar o estoque
-- (o motor de virada + teste é da Etapa 1.2; aqui só o schema).
CREATE TABLE IF NOT EXISTS lots (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id    uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    name        text NOT NULL,
    price_cents bigint NOT NULL,
    stock       integer NOT NULL,
    sold        integer NOT NULL DEFAULT 0,
    starts_at   timestamptz,
    ends_at     timestamptz,
    position    integer NOT NULL DEFAULT 0,
    status      text NOT NULL DEFAULT 'scheduled'
                  CHECK (status IN ('scheduled', 'active', 'sold_out', 'closed')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- sectors — setor do evento. table = mesa/camarote (unidade de venda configurável).
CREATE TABLE IF NOT EXISTS sectors (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id      uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    name          text NOT NULL,
    kind          text NOT NULL CHECK (kind IN ('standing', 'seated', 'table')),
    -- para setor 'table': a unidade de venda é a mesa inteira ou a cadeira avulsa.
    sell_unit     text CHECK (sell_unit IN ('seat', 'table')),
    rows          integer,
    seats_per_row integer,
    -- regra de nomeação (fileira A-Z ou 1-N; par/ímpar ou sequencial; corredores).
    naming_rule   jsonb NOT NULL DEFAULT '{}',
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- seats — assento individual, com bloqueio e motivo (poltrona quebrada, técnica,
-- imprensa, artista). blocked_reason nullable = assento vendável.
CREATE TABLE IF NOT EXISTS seats (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    sector_id      uuid NOT NULL REFERENCES sectors(id) ON DELETE CASCADE,
    row_label      text,
    number         text,
    blocked_reason text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (sector_id, row_label, number)
);

-- sector_price_rules — preço por lote × setor (plateia e balcão com valores
-- distintos no mesmo lote). half_price_cents opcional para meia-entrada.
CREATE TABLE IF NOT EXISTS sector_price_rules (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    lot_id           uuid NOT NULL REFERENCES lots(id) ON DELETE CASCADE,
    sector_id        uuid NOT NULL REFERENCES sectors(id) ON DELETE CASCADE,
    price_cents      bigint NOT NULL,
    half_price_cents bigint,
    UNIQUE (lot_id, sector_id)
);

-- venue_templates — modelos de mapa reaproveitáveis (teatro italiano, auditório,
-- casa com mesas), editáveis por evento.
CREATE TABLE IF NOT EXISTS venue_templates (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    layout     jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);
