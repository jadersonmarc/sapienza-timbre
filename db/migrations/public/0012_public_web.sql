-- Camada pública (Onda 1): superfície do comprador. Enriquece o event_directory para o
-- diretório/SEO, cria o índice de ingressos do comprador (para "meus ingressos" sem varrer
-- schemas) e a tabela de códigos de acesso (OTP). Tudo em `public` (cross-produtor), coerente
-- com a identidade/audiência que já vive aqui.

-- event_directory ganha os campos que o card do diretório e o SEO precisam. São
-- DENORMALIZADOS e sincronizados pelo produtor no publish e na virada de lote (o min_price
-- muda quando o lote esgota). Nenhum é dado sensível.
ALTER TABLE event_directory ADD COLUMN IF NOT EXISTS cover_url       text;
ALTER TABLE event_directory ADD COLUMN IF NOT EXISTS city            text;
ALTER TABLE event_directory ADD COLUMN IF NOT EXISTS lat             double precision;
ALTER TABLE event_directory ADD COLUMN IF NOT EXISTS lng             double precision;
ALTER TABLE event_directory ADD COLUMN IF NOT EXISTS min_price_cents bigint;
ALTER TABLE event_directory ADD COLUMN IF NOT EXISTS updated_at      timestamptz NOT NULL DEFAULT now();
CREATE INDEX IF NOT EXISTS event_directory_city_idx     ON event_directory (city);
CREATE INDEX IF NOT EXISTS event_directory_starts_idx   ON event_directory (starts_at);
CREATE INDEX IF NOT EXISTS event_directory_category_idx ON event_directory (category);

-- payment_index já mapeia asaas_ref -> (producer, order). A tela de espera do Pix consulta o
-- status por order_id, então indexamos essa coluna.
CREATE INDEX IF NOT EXISTS payment_index_order_idx ON payment_index (order_id);

-- ticket_directory: índice público dos ingressos de um comprador, para "meus ingressos" sem
-- varrer os schemas de produtor. subject_id fica nulo na compra como convidado e é preenchido
-- quando o comprador cria conta com o MESMO e-mail e VERIFICA o OTP (vínculo retroativo). O
-- snapshot (title/starts_at/city) evita ler o tenant só para listar. token = QR assinado.
CREATE TABLE IF NOT EXISTS ticket_directory (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_id       uuid REFERENCES subjects(id) ON DELETE SET NULL,
    buyer_email      text,
    producer_id      uuid NOT NULL REFERENCES producers(id) ON DELETE CASCADE,
    event_id         uuid NOT NULL,      -- referência solta (evento no schema do produtor)
    event_title      text NOT NULL,
    event_starts_at  timestamptz,
    venue_city       text,
    ticket_id        uuid NOT NULL,      -- referência solta (ticket no schema do produtor)
    token            text,              -- QR assinado (Ed25519), sem dado pessoal
    seat_label       text,
    status           text NOT NULL DEFAULT 'active',
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (producer_id, ticket_id)
);
CREATE INDEX IF NOT EXISTS ticket_directory_subject_idx ON ticket_directory (subject_id);
CREATE INDEX IF NOT EXISTS ticket_directory_email_idx   ON ticket_directory (lower(buyer_email));

-- buyer_otps: códigos de acesso do comprador. code_hash = bcrypt (nunca o código em claro).
-- Uso único (consumed_at), invalidado ao emitir novo, com limite por e-mail e por IP.
CREATE TABLE IF NOT EXISTS buyer_otps (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email        text NOT NULL,
    code_hash    text NOT NULL,
    expires_at   timestamptz NOT NULL,
    consumed_at  timestamptz,
    attempts     integer NOT NULL DEFAULT 0,
    requested_ip text,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS buyer_otps_email_idx ON buyer_otps (lower(email), created_at DESC);
