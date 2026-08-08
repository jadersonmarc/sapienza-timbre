-- Gestão do ingresso como NFT (Etapa 1.9). Custódia e disputa por ingresso.

-- Custódia: platform (carteira MPC gerida) ou external (exportada — o participante
-- assume a custódia). exported_at registra o evento de exportação. Exportar NÃO muda o
-- status nem a assinatura: a validade do QR e a entrada na portaria seguem intactas.
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS custody text NOT NULL DEFAULT 'platform'
    CHECK (custody IN ('platform', 'external'));
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS exported_at timestamptz;

-- Disputa bloqueia a TRANSFERÊNCIA (não a entrada). No máximo uma disputa aberta por
-- ingresso.
CREATE TABLE IF NOT EXISTS ticket_disputes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_id   uuid NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    reason      text,
    status      text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS ticket_disputes_open_key ON ticket_disputes (ticket_id) WHERE status = 'open';
