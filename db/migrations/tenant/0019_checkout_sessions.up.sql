-- Sessão de checkout: a seleção sobrevive ao desvio de autenticação. O comprador escolhe
-- lote/quantidade/assentos/cupom/meia SEM conta; a conta é exigida no momento de pagar.
-- A reserva (hold de assentos ou held_count do lote) é criada aqui e atravessa o acesso.
CREATE TABLE IF NOT EXISTS checkout_sessions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id      uuid NOT NULL REFERENCES events(id),
    anon_token    text NOT NULL,               -- identificador do navegador (reutilizável)
    subject_id    uuid,                      -- referência solta a public.subjects (preenchida no bind)
    client_ip     text,                      -- p/ teto de sessões abertas por IP (expurgado após fechar)
    items         jsonb NOT NULL,            -- lote, quantidade, assentos, cupom, meia-entrada
    hold_id       uuid,                      -- referência solta a holds (assento marcado)
    status        text NOT NULL DEFAULT 'open'
                    CHECK (status IN ('open', 'authenticated', 'paid', 'expired', 'abandoned')),
    grace_applied boolean NOT NULL DEFAULT false, -- a extensão do bind é única por sessão
    expires_at    timestamptz NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS checkout_sessions_status_idx ON checkout_sessions (status, expires_at);
CREATE INDEX IF NOT EXISTS checkout_sessions_anon_idx ON checkout_sessions (anon_token, status, event_id);
CREATE INDEX IF NOT EXISTS checkout_sessions_ip_idx ON checkout_sessions (client_ip) WHERE status IN ('open','authenticated');
