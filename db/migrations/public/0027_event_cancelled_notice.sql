-- Aviso de evento cancelado. Sai na TRANSIÇÃO, não no fim da devolução: quem tinha ingresso
-- para amanhã precisa saber hoje, mesmo que o dinheiro leve dias para voltar.
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_kind_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_kind_check
    CHECK (kind IN ('auth_code', 'ticket_issued', 'order_refunded', 'waitlist',
                    'refund_requested', 'refund_rejected', 'event_cancelled'));
