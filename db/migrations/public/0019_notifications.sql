-- Registro de notificações enviadas. Fica em `public` (compartilhado) e NÃO é por-tenant:
-- o código de acesso (auth_code) é disparado sem contexto de produtor, e o painel do
-- produtor agrega por producer_id. producer_id/event_id são referências soltas.
CREATE TABLE IF NOT EXISTS notifications (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    producer_id         uuid,   -- referência solta a public.producers (null p/ auth_code)
    event_id            uuid,   -- referência solta (evento no schema do produtor)
    kind                text NOT NULL
                          CHECK (kind IN ('auth_code', 'ticket_issued', 'order_refunded', 'waitlist')),
    to_email            text NOT NULL,
    subject_id          uuid,   -- referência solta a public.subjects
    ticket_id           uuid,   -- referência solta
    order_id            uuid,   -- referência solta
    payload             jsonb NOT NULL, -- subject/text/html/anexo renderizados
    provider_message_id text,
    status              text NOT NULL DEFAULT 'queued'
                          CHECK (status IN ('queued', 'sent', 'failed')),
    attempts            integer NOT NULL DEFAULT 0,
    last_error          text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    sent_at             timestamptz
);
CREATE INDEX IF NOT EXISTS notifications_status_idx ON notifications (status, created_at);
CREATE INDEX IF NOT EXISTS notifications_producer_idx ON notifications (producer_id, event_id, created_at);
