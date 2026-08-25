-- Índice público dos pedidos do comprador. Pedidos vivem no schema de cada produtor, e uma
-- pessoa compra de vários — sem este índice, "meus pedidos" só existiria varrendo todos os
-- schemas a cada abertura de tela.
--
-- Mesmo desenho do ticket_directory: snapshot do que a tela precisa mostrar (nome e data do
-- evento) para listar sem tocar o tenant, e a decomposição do valor como ela foi cobrada —
-- é a resposta para "por que paguei isso?" numa contestação, meses depois, mesmo que o
-- preço do evento tenha mudado.
CREATE TABLE IF NOT EXISTS order_directory (
    producer_id     uuid NOT NULL REFERENCES producers(id) ON DELETE CASCADE,
    order_id        uuid NOT NULL,
    subject_id      uuid REFERENCES subjects(id) ON DELETE SET NULL,
    buyer_email     text,
    event_id        uuid NOT NULL,
    event_title     text NOT NULL,
    event_starts_at timestamptz,
    ticket_count    int  NOT NULL DEFAULT 0,
    face_cents      bigint NOT NULL DEFAULT 0,
    fee_cents       bigint NOT NULL DEFAULT 0,
    total_cents     bigint NOT NULL DEFAULT 0,
    method          text,
    installments    int NOT NULL DEFAULT 1,
    status          text NOT NULL DEFAULT 'pending',
    asaas_ref       text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    paid_at         timestamptz,
    refunded_at     timestamptz,
    PRIMARY KEY (producer_id, order_id)
);

CREATE INDEX IF NOT EXISTS order_directory_subject_idx ON order_directory (subject_id, created_at DESC);
CREATE INDEX IF NOT EXISTS order_directory_email_idx   ON order_directory (lower(buyer_email));
