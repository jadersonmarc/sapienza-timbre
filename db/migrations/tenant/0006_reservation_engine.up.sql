-- Motor de reserva (Etapa 1.3). Reforma o modelo de holds e introduz a tabela de
-- ocupação de assento que dá a garantia cross-table num ÚNICO índice.
--
-- Antes (0002): holds tinha seat_id (uma linha por assento) e um índice só cobria
-- hold×hold. Não havia como garantir hold×ticket num índice (duas tabelas).
--
-- Agora: `holds` é a RESERVA (grupo de N assentos). A ocupação por assento — seja um
-- hold vivo, seja um ingresso emitido — vive em `seat_occupancy`, e UM índice único
-- parcial (event_id, seat_id) WHERE NOT released garante que um assento tenha no
-- máximo UMA ocupação viva: cobre hold×hold, ticket×ticket E hold×ticket de uma vez.

-- holds vira o grupo: remove a coluna por-assento e seu índice.
DROP INDEX IF EXISTS holds_live_seat_key;
ALTER TABLE holds DROP COLUMN IF EXISTS seat_id;

CREATE TABLE IF NOT EXISTS seat_occupancy (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id   uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    seat_id    uuid NOT NULL REFERENCES seats(id) ON DELETE CASCADE,
    kind       text NOT NULL CHECK (kind IN ('hold', 'ticket')),
    hold_id    uuid REFERENCES holds(id) ON DELETE CASCADE,
    ticket_id  uuid REFERENCES tickets(id) ON DELETE CASCADE,
    expires_at timestamptz,   -- só para kind='hold'; NULL para ticket
    released   boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- INVARIANTE central da 1.3 (garantia de schema, não da aplicação): no máximo UMA
-- ocupação viva por assento. É o que faz N compradores disputando o mesmo assento
-- terem exatamente um vencedor.
CREATE UNIQUE INDEX IF NOT EXISTS seat_occupancy_live_key
    ON seat_occupancy (event_id, seat_id) WHERE NOT released;

-- A varredura de expiração pega holds vencidos por aqui.
CREATE INDEX IF NOT EXISTS seat_occupancy_expiry_idx
    ON seat_occupancy (expires_at) WHERE kind = 'hold' AND NOT released;

-- Regra anti-buraco, opcional por evento (impede deixar um assento isolado entre dois
-- ocupados). Desligada por default.
ALTER TABLE events ADD COLUMN IF NOT EXISTS anti_hole boolean NOT NULL DEFAULT false;
