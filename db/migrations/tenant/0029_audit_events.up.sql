-- Trilha de auditoria do produtor, generalizada.
--
-- Ela nasceu presa ao pedido de estorno (refund_request_events). Agora as ações de INGRESSO
-- — reemissão, transferência pelo produtor — precisam da mesma coisa: data, hora, ator,
-- motivo, append-only. Criar uma segunda tabela para o mesmo propósito significaria dois
-- lugares para procurar quando alguém contesta, e duas chances de esquecer de gravar.
--
-- Então a existente vira geral, em vez de ganhar uma irmã.
ALTER TABLE refund_request_events RENAME TO audit_events;

-- O vínculo com o pedido de estorno deixa de ser obrigatório: uma reemissão não tem pedido.
ALTER TABLE audit_events ALTER COLUMN request_id DROP NOT NULL;

-- Sobre o que é a linha. As já existentes são todas de pedido de estorno.
ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS entity text NOT NULL DEFAULT 'refund_request';
ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS audit_events_entity_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_entity_check
    CHECK (entity IN ('refund_request', 'ticket', 'courtesy', 'export'));

ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS ticket_id uuid;
-- Detalhe estruturado do que mudou (o e-mail anterior numa reemissão, a categoria antiga
-- numa reclassificação). Sem isso, "o que exatamente mudou" só existiria na cabeça de quem
-- fez.
ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS details jsonb NOT NULL DEFAULT '{}';

CREATE INDEX IF NOT EXISTS audit_events_ticket_idx ON audit_events (ticket_id, at)
    WHERE ticket_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS audit_events_entity_idx ON audit_events (entity, at DESC);
