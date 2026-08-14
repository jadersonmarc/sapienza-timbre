-- 0013_catalog_v2 — reconcilia o catálogo ao spec autoritativo da Etapa 1.2.
--
-- IRREVERSÍVEL: o runner do kit (tenancy.MigrationRunner) só aplica *.up.sql — não há
-- down. Esta migração remove o modelo antigo de lote (status/stock/sold) e o CHECK enum
-- de categoria. Esse estado era DERIVADO de um modelo que deixa de existir e NÃO é
-- recuperável — reverter exige restore de backup. Não finja reversão.
--
-- O que muda: (a) categoria vira tabela event_categories + FK; (b) status de evento ganha
-- 'pending_review'; (c) lote passa a contadores (quantity/sold_count/held_count) com CHECK
-- e vigência derivada (sem coluna status). seat_occupancy (0006) já cobre o assento.

-- ── categorias: enum CHECK → tabela + FK ──────────────────────────────────────
CREATE TABLE IF NOT EXISTS event_categories (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug       text UNIQUE NOT NULL,
    name       text NOT NULL,
    sort_order int NOT NULL DEFAULT 0,
    active     boolean NOT NULL DEFAULT true
);

-- Seed idempotente das 8 categorias iniciais (mesmos slugs do enum antigo).
INSERT INTO event_categories (slug, name, sort_order) VALUES
    ('shows',       'Shows',       1),
    ('teatro',      'Teatro',      2),
    ('festas',      'Festas',      3),
    ('esportes',    'Esportes',    4),
    ('congressos',  'Congressos',  5),
    ('cursos',      'Cursos',      6),
    ('workshops',   'Workshops',   7),
    ('gastronomia', 'Gastronomia', 8)
ON CONFLICT (slug) DO NOTHING;

-- events.category_id (FK) + backfill pelo slug denormalizado; depois NOT NULL.
ALTER TABLE events ADD COLUMN IF NOT EXISTS category_id uuid REFERENCES event_categories(id);
UPDATE events e SET category_id = c.id
  FROM event_categories c WHERE c.slug = e.category AND e.category_id IS NULL;
ALTER TABLE events ALTER COLUMN category_id SET NOT NULL;

-- A categoria passa a ser validada por FK + event_categories.active; solta o CHECK enum.
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_category_check;

-- Ciclo de vida ganha 'pending_review'.
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_status_check;
ALTER TABLE events ADD CONSTRAINT events_status_check
    CHECK (status IN ('draft','pending_review','published','suspended','finished','cancelled'));

-- ── lotes: status/stock/sold → quantity/sold_count/held_count + CHECK ──────────
-- Pré-verificação ANTES do CHECK, com erro que diz o que está errado (Correção 4.1).
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM lots WHERE sold > stock) THEN
        RAISE EXCEPTION 'lote com sold > stock impede a migration 0013: corrigir os dados antes de aplicar';
    END IF;
END $$;

ALTER TABLE lots RENAME COLUMN position TO sort_order;

ALTER TABLE lots ADD COLUMN IF NOT EXISTS quantity int;
UPDATE lots SET quantity = stock WHERE quantity IS NULL;
ALTER TABLE lots ALTER COLUMN quantity SET NOT NULL;

ALTER TABLE lots ADD COLUMN IF NOT EXISTS sold_count int NOT NULL DEFAULT 0;
UPDATE lots SET sold_count = sold;
ALTER TABLE lots ADD COLUMN IF NOT EXISTS held_count int NOT NULL DEFAULT 0;

-- ENTREGÁVEL CENTRAL: o schema, não a aplicação, garante que nunca se venda além do lote.
ALTER TABLE lots ADD CONSTRAINT lots_capacity_chk CHECK (sold_count + held_count <= quantity);

ALTER TABLE lots DROP CONSTRAINT IF EXISTS lots_status_check;
ALTER TABLE lots DROP COLUMN IF EXISTS status;
ALTER TABLE lots DROP COLUMN IF EXISTS stock;
ALTER TABLE lots DROP COLUMN IF EXISTS sold;

-- sort_order único por evento (a fila de virada não admite empate).
CREATE UNIQUE INDEX IF NOT EXISTS lots_event_sortorder_key ON lots (event_id, sort_order);
