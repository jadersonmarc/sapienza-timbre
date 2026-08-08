-- Divulgação e engajamento (Etapa 2.8) por produtor.

-- Pixels do evento (Meta/Google) para a página pública de checkout.
ALTER TABLE events ADD COLUMN IF NOT EXISTS meta_pixel text;
ALTER TABLE events ADD COLUMN IF NOT EXISTS google_id text;

-- Campanhas com UTM + contagem de cliques.
CREATE TABLE IF NOT EXISTS campaigns (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id     uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    name         text NOT NULL,
    utm_source   text,
    utm_medium   text,
    utm_campaign text,
    clicks       integer NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Atribuição da compra à campanha.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS campaign_id uuid REFERENCES campaigns(id);

-- Lista de espera: quando o evento/lote esgota, avisa na virada (notified_at).
CREATE TABLE IF NOT EXISTS waitlist (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id    uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    email       text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    notified_at timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS waitlist_event_email_key ON waitlist (event_id, email);
