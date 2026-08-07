-- Financeiro por produtor: repasse, retenção, conciliação e pagamentos ao produtor.

-- ledger_entries — cobre repasse (D+2 após a realização), retenção (5% por 60 dias
-- em cartão, como reserva para contestação) e conciliação do split.
CREATE TABLE IF NOT EXISTS ledger_entries (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id     uuid REFERENCES events(id),
    order_id     uuid REFERENCES orders(id),
    payment_id   uuid REFERENCES payments(id),
    kind         text NOT NULL
                   CHECK (kind IN ('repasse', 'retencao', 'conciliacao', 'estorno', 'taxa')),
    amount_cents bigint NOT NULL,
    -- quando o valor fica disponível (D+2 no repasse; +60d na retenção de cartão).
    available_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ledger_entries_available_idx ON ledger_entries (available_at);

-- payouts — o repasse efetivo ao produtor (via Asaas).
CREATE TABLE IF NOT EXISTS payouts (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    amount_cents  bigint NOT NULL,
    asaas_ref     text,
    status        text NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'sent', 'failed')),
    scheduled_for timestamptz,
    sent_at       timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);
