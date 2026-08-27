-- Política de estorno e o pedido do comprador.
--
-- Até aqui só o produtor e o admin estornavam, e a regra vivia na cabeça de quem decidia.
-- Isto dá à decisão um lugar: o que a casa promete, quem pediu, por qual trilha, quem
-- decidiu e quando.

-- ── política ──────────────────────────────────────────────────────────────────
-- Uma linha por evento; a linha com event_id NULL é o default do produtor, herdado por
-- evento que não define a sua. Sem o default, cada evento novo começaria sem promessa
-- nenhuma e a resposta ao comprador dependeria de quem atendesse.
CREATE TABLE IF NOT EXISTS refund_policies (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id  uuid REFERENCES events(id) ON DELETE CASCADE,

    -- Janela de arrependimento, contada da COMPRA. O piso de 7 dias é o direito de
    -- arrependimento do art. 49 do CDC: o produtor pode oferecer MAIS, nunca menos, e é o
    -- CHECK que garante isso — deixar para a aplicação seria deixar para o esquecimento.
    withdrawal_window_days int NOT NULL DEFAULT 7 CHECK (withdrawal_window_days >= 7),

    -- Antecedência mínima em relação ao evento. Zero = sem exigência. Existe porque
    -- devolver ingresso com a casa montando não é a mesma coisa que devolver na semana da
    -- compra, e a diferença precisa ser dita antes, não na recusa.
    withdrawal_min_hours_before_event int NOT NULL DEFAULT 0 CHECK (withdrawal_min_hours_before_event >= 0),

    -- Quem absorve a tarifa que o gateway retém e não devolve. Default: a plataforma.
    refund_gateway_fee_bearer text NOT NULL DEFAULT 'platform'
        CHECK (refund_gateway_fee_bearer IN ('platform', 'producer')),

    -- Se o produtor aceita analisar pedido FORA da janela (liberalidade). Desligado, o
    -- pedido fora da janela é recusado na hora, com o motivo — melhor que ficar parado
    -- numa fila que ninguém vai olhar.
    producer_discretionary_enabled boolean NOT NULL DEFAULT true,

    -- Prazo para o produtor responder um pedido de liberalidade. Vencido, o pedido aparece
    -- como atrasado na fila. NÃO aprova sozinho: silêncio não é consentimento, e aprovação
    -- automática moveria dinheiro sem ninguém decidir.
    discretionary_response_hours int NOT NULL DEFAULT 72 CHECK (discretionary_response_hours > 0),

    -- Entrada registrada bloqueia o estorno (quem entrou consumiu o serviço). O admin passa
    -- por cima disso de qualquer forma.
    checkin_blocks_refund boolean NOT NULL DEFAULT true,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Uma política por evento, e uma só default. Índices parciais porque NULL não colide
-- consigo mesmo num UNIQUE comum — sem o segundo, dava para criar dois defaults.
CREATE UNIQUE INDEX IF NOT EXISTS refund_policies_event_key
    ON refund_policies (event_id) WHERE event_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS refund_policies_default_key
    ON refund_policies ((true)) WHERE event_id IS NULL;

-- ── pedido de estorno ─────────────────────────────────────────────────────────
-- Separado de `refunds`, que é a EXECUÇÃO. Um pedido pode ser recusado e nunca virar
-- execução; e uma execução (o produtor cancelando por conta própria) nasce sem pedido do
-- comprador. Juntar os dois faria um dos dois casos caber mal.
CREATE TABLE IF NOT EXISTS refund_requests (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id   uuid NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    -- Vazio = pedido do pedido inteiro.
    ticket_ids uuid[] NOT NULL DEFAULT '{}',

    requested_by      text NOT NULL CHECK (requested_by IN ('buyer', 'producer', 'admin')),
    requested_subject uuid, -- comprador que pediu (referência solta a public.subjects)

    -- A trilha decide QUEM autoriza, e é derivada da política no momento do pedido — não
    -- escolhida por quem pede.
    --   withdrawal         — comprador dentro da janela: direito, não favor. Não passa pelo produtor.
    --   discretionary      — comprador fora da janela: liberalidade, vai para a fila.
    --   producer_initiated — o produtor cancelando, sem pedido do comprador.
    --   admin_override     — a plataforma por cima de tudo, com motivo.
    track text NOT NULL CHECK (track IN ('withdrawal', 'discretionary', 'producer_initiated', 'admin_override')),

    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'approved', 'rejected', 'processing', 'completed', 'failed')),

    reason      text,
    decided_by  text,   -- quem decidiu, por papel + identificação
    decided_at  timestamptz,
    -- Motivo da recusa. Recusar sem dizer por quê é o que faz o comprador voltar pelo
    -- canal mais caro.
    decision_reason text,

    -- Prazo de resposta da liberalidade. Vencido, aparece como atrasado.
    responds_by timestamptz,

    -- Preenchidos quando a execução acontece.
    refund_id         uuid REFERENCES refunds(id),
    face_cents        bigint NOT NULL DEFAULT 0,
    convenience_cents bigint NOT NULL DEFAULT 0,
    refund_amount_cents bigint NOT NULL DEFAULT 0,
    error             text,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS refund_requests_order_idx  ON refund_requests (order_id);
CREATE INDEX IF NOT EXISTS refund_requests_status_idx ON refund_requests (status, created_at);

-- INVARIANTE (por índice): um pedido vivo por ordem. Sem isso, o comprador clicando duas
-- vezes cria duas solicitações e o produtor decide a mesma coisa em duplicado.
CREATE UNIQUE INDEX IF NOT EXISTS refund_requests_open_key
    ON refund_requests (order_id) WHERE status IN ('pending', 'approved', 'processing');

-- ── auditoria ─────────────────────────────────────────────────────────────────
-- Append-only de propósito: é o que o produtor mostra quando o comprador reclama, e o que
-- a plataforma mostra quando o produtor reclama. Decisão tomada não se edita — uma decisão
-- que muda vira outra linha.
CREATE TABLE IF NOT EXISTS refund_request_events (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id  uuid NOT NULL REFERENCES refund_requests(id) ON DELETE CASCADE,
    at          timestamptz NOT NULL DEFAULT now(),
    actor_kind  text NOT NULL CHECK (actor_kind IN ('buyer', 'producer', 'admin', 'system')),
    actor       text,
    from_status text,
    to_status   text NOT NULL,
    reason      text
);

CREATE INDEX IF NOT EXISTS refund_request_events_request_idx ON refund_request_events (request_id, at);
