-- Aviso de CORTESIA. Existe separado do ticket_issued porque quem recebe não comprou nada:
-- o texto começa dizendo QUEM enviou, e termina explicando por que aquele endereço foi
-- usado. É dado pessoal de terceiro que entrou no sistema pela mão do produtor.
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_kind_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_kind_check
    CHECK (kind IN ('auth_code', 'ticket_issued', 'order_refunded', 'waitlist',
                    'refund_requested', 'refund_rejected', 'event_cancelled', 'courtesy_issued'));
