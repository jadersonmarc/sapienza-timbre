-- A bilheteria retém e repassa depois do evento.
--
-- O modelo de split direto ao produtor na própria cobrança está DESCARTADO. Ele tinha um
-- defeito que nenhuma correção de código resolve: se o evento não acontece, o dinheiro já
-- está na conta de quem não vai devolvê-lo. Volta o modelo de mercado — o valor integral cai
-- na conta da bilheteria, e o repasse acontece depois da realização.
--
-- Esta migração é, na maior parte, REMOÇÃO. O que entra no lugar é uma linha por evento
-- dizendo quanto a plataforma deve, a quem e com que vencimento.

-- ── o que sai ─────────────────────────────────────────────────────────────────
-- split_transfers e split_refunds eram máquinas de estado do repasse no gateway. Deixá-las
-- de pé sem escritor seria pior que removê-las: um estado parado é lido como estado real
-- pela próxima pessoa que olhar, e o caminho volta sozinho.
DROP TABLE IF EXISTS split_refunds;
DROP TABLE IF EXISTS split_transfers;

-- payouts materializava o líquido do razão em ordem de pagamento. A obrigação passa a ser
-- por EVENTO (abaixo), com vencimento — e uma segunda fila de pagamento seria a mesma
-- conta em dois lugares.
DROP TABLE IF EXISTS payouts;

-- payments.split guardava a divisão combinada no ato. Não há divisão.
ALTER TABLE payments DROP COLUMN IF EXISTS split;

-- settled_by distinguia "quem entregou o face ao produtor": o gateway (split) ou a
-- plataforma. Só existe uma resposta agora.
ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_entries_settled_by_check;
ALTER TABLE ledger_entries DROP COLUMN IF EXISTS settled_by;

-- 'retencao' era a reserva de contestação de 5%/60d sobre o repasse do PRODUTOR. Com a
-- retenção até o evento, a reserva é da plataforma por construção: o dinheiro não saiu.
-- O kind continua no CHECK porque as linhas já lançadas continuam sendo história — o que
-- acaba é a escrita de novas.
COMMENT ON COLUMN ledger_entries.kind IS
    'repasse|taxa|processamento|estorno|estorno_taxa|conciliacao. ''retencao'' é histórico: a reserva de contestação deixou de ser do produtor quando a bilheteria passou a reter até o evento.';

-- ── parâmetro do prazo ────────────────────────────────────────────────────────
-- Mesmo desenho da política de devolução: a linha com event_id NULL é o default da casa,
-- e o evento pode ter a própria. Nada chumbado no código — o default abaixo é o do banco.
CREATE TABLE IF NOT EXISTS payout_settings (
    id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id uuid REFERENCES events(id) ON DELETE CASCADE,

    -- Dias entre a REALIZAÇÃO do evento e o vencimento do repasse.
    payout_delay_days int NOT NULL DEFAULT 7 CHECK (payout_delay_days >= 0),

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS payout_settings_event_key
    ON payout_settings (event_id) WHERE event_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS payout_settings_default_key
    ON payout_settings ((true)) WHERE event_id IS NULL;

-- ── a obrigação ───────────────────────────────────────────────────────────────
-- Uma linha por evento. Enquanto o evento não acontece ela fica 'accruing' e cada venda,
-- cortesia ou estorno a atualiza; na realização vira 'pending' com vencimento.
--
-- O razão continua sendo contabilidade (o que aconteceu). Isto é OBRIGAÇÃO: quanto, a quem
-- e até quando. São coisas diferentes, e enquanto foram a mesma o produtor não tinha como
-- saber quando ia receber.
CREATE TABLE IF NOT EXISTS event_payouts (
    event_id uuid PRIMARY KEY REFERENCES events(id) ON DELETE CASCADE,

    gross_face_cents    bigint NOT NULL DEFAULT 0, -- soma das faces vendidas
    refunded_face_cents bigint NOT NULL DEFAULT 0, -- faces estornadas
    platform_fee_cents  bigint NOT NULL DEFAULT 0, -- conveniência: é da plataforma, não entra no líquido
    -- Tarifa que o gateway reteve nas devoluções e não devolveu. Só sai do produtor quando
    -- a política do evento diz que ele a absorve (refund_gateway_fee_bearer='producer').
    gateway_fee_cents   bigint NOT NULL DEFAULT 0,
    net_due_cents       bigint NOT NULL DEFAULT 0, -- o que o produtor recebe

    -- Vencimento, calculado da realização do evento + payout_settings.payout_delay_days.
    -- Nulo enquanto o evento não aconteceu: prometer data antes disso seria inventar.
    due_at timestamptz,

    status text NOT NULL DEFAULT 'accruing'
        CHECK (status IN ('accruing', 'pending', 'on_hold', 'paid', 'cancelled')),

    -- Retenção: motivo e ator ficam na linha porque "está retido" sem porquê é
    -- indistinguível, para quem espera o dinheiro, de "esqueceram de mim".
    hold_reason text,
    hold_actor  text,
    held_at     timestamptz,

    -- A EXECUÇÃO BANCÁRIA NÃO EXISTE AQUI. Marcar como pago é ação manual do admin, e a
    -- referência é o comprovante: sem ela, "pago" vira palavra contra palavra.
    paid_at        timestamptz,
    paid_reference text,
    paid_by        text,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS event_payouts_status_idx ON event_payouts (status, due_at);

-- ── crédito a recuperar ───────────────────────────────────────────────────────
-- Estorno depois do repasse LIQUIDADO: o dinheiro do evento já foi para o produtor e o
-- comprador tem de ser devolvido assim mesmo. Vira crédito da plataforma contra o produtor.
--
-- NÃO é abatido de nada automaticamente. Não há repasse futuro garantido — produtor que
-- fez um evento e parou nunca teria de onde descontar, e um abatimento automático
-- apareceria como repasse a menos, sem explicação, no evento seguinte de quem continuou.
CREATE TABLE IF NOT EXISTS recoverable_credits (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id   uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    order_id   uuid NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    refund_id  uuid NOT NULL REFERENCES refunds(id) ON DELETE CASCADE,

    amount_cents bigint NOT NULL,
    reason       text,

    -- Preenchidos quando alguém resolve: acerto por fora, desconto combinado, perdão.
    settled_at        timestamptz,
    settled_by        text,
    settlement_note   text,

    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS recoverable_credits_refund_key ON recoverable_credits (refund_id);
CREATE INDEX IF NOT EXISTS recoverable_credits_open_idx ON recoverable_credits (created_at) WHERE settled_at IS NULL;
