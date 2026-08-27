-- O ciclo do pedido de estorno tem três avisos distintos, e eles dizem coisas diferentes:
-- "recebemos", "não vamos devolver e este é o motivo" e "o dinheiro voltou". Juntar os três
-- num só faria o comprador ler a recusa como confirmação.
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_kind_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_kind_check
    CHECK (kind IN ('auth_code', 'ticket_issued', 'order_refunded', 'waitlist',
                    'refund_requested', 'refund_rejected'));
