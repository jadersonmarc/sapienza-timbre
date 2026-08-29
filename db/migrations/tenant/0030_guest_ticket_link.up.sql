-- A cortesia e o ingresso que ela emitiu nunca ficaram ligados: guest_list_entries guarda
-- o convidado e a categoria, tickets guarda o ingresso, e a única ponte era o assento — que
-- não existe em evento de pista. Sem a ponte, uma exportação por ingresso não consegue dizer
-- de qual categoria a cortesia é, e a categoria é o que o atestado publica.
ALTER TABLE guest_list_entries ADD COLUMN IF NOT EXISTS ticket_id uuid REFERENCES tickets(id);

-- Backfill do que dá para reconstruir com certeza: cortesia com assento casa com o ingresso
-- ativo daquele assento. Cortesia de pista antiga fica sem ponte — inventar um par por
-- ordem de criação erraria calado, e errado é pior que vazio numa trilha de comprovação.
UPDATE guest_list_entries g
   SET ticket_id = t.id
  FROM tickets t
 WHERE g.ticket_id IS NULL
   AND g.seat_id IS NOT NULL
   AND t.event_id = g.event_id
   AND t.seat_id  = g.seat_id
   AND t.order_id IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS guest_list_entries_ticket_key
    ON guest_list_entries (ticket_id) WHERE ticket_id IS NOT NULL;
