-- Categorias de cortesia (por tenant). A emissão de cortesia passa a exigir categoria; o
-- relatório de contrapartida compara o declarado com o realizado POR categoria.
CREATE TABLE IF NOT EXISTS courtesy_categories (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug       text NOT NULL UNIQUE,
    name       text NOT NULL,
    sort_order integer NOT NULL DEFAULT 0,
    active     boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO courtesy_categories (slug, name, sort_order) VALUES
    ('comunidade', 'Comunidade', 1),
    ('escola_publica', 'Escola pública', 2),
    ('imprensa', 'Imprensa', 3),
    ('convidado', 'Convidado', 4),
    ('patrocinador', 'Patrocinador', 5),
    ('equipe', 'Equipe', 6),
    ('outro', 'Outro', 7)
ON CONFLICT (slug) DO NOTHING;

-- guest_list_entries ganha categoria; backfill aponta o default para 'outro'.
ALTER TABLE guest_list_entries ADD COLUMN IF NOT EXISTS courtesy_category_id uuid;
UPDATE guest_list_entries SET courtesy_category_id = (SELECT id FROM courtesy_categories WHERE slug = 'outro')
    WHERE courtesy_category_id IS NULL;
ALTER TABLE guest_list_entries ALTER COLUMN courtesy_category_id SET NOT NULL;
ALTER TABLE guest_list_entries
    ADD CONSTRAINT guest_list_entries_category_fk FOREIGN KEY (courtesy_category_id) REFERENCES courtesy_categories(id);

-- Compromissos declarados pelo organizador (contrapartida). A comparação entre o
-- declarado e o realizado é o produto — sem compromisso, o relatório sai vazio.
CREATE TABLE IF NOT EXISTS event_commitments (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id             uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    kind                 text NOT NULL
                           CHECK (kind IN ('courtesy_share', 'meia_entrada_cota', 'free_admission', 'custom')),
    courtesy_category_id uuid REFERENCES courtesy_categories(id),
    target_type          text NOT NULL CHECK (target_type IN ('percent', 'absolute')),
    target_value         numeric NOT NULL,
    description          text,
    created_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS event_commitments_event_idx ON event_commitments (event_id);
