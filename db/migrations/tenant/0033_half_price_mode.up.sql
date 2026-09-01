-- Meia-entrada: 40% volta a ser DEFAULT, não trava.
--
-- A trava no piso legal foi uma decisão nossa sobre um risco que é do produtor. A Lei
-- 12.933/2013 obriga ele, e recusar a configuração dele não o faz cumprir a lei — só o
-- impede de operar e nos coloca no lugar de fiscal. O que o sistema deve fazer é o que um
-- sistema pode fazer: mostrar a regra, avisar quando a escolha fica abaixo dela, e REGISTRAR
-- quem escolheu, quando e quanto.

-- Modo da meia no evento:
--   quota  — a meia tem cota própria (percentual ou absoluta), declarada em event_commitments
--   linked — a meia é vinculada à inteira: segue o estoque do lote pai, sem limite próprio
--
-- O modo 'linked' é para quem não quer administrar cota: a meia sai enquanto houver ingresso.
-- Continua consumindo o estoque do tipo pai — estoque próprio que SOMA fica fora, porque é
-- o que faz a casa ser vendida duas vezes.
ALTER TABLE events ADD COLUMN IF NOT EXISTS half_price_mode text NOT NULL DEFAULT 'quota';
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_half_price_mode_check;
ALTER TABLE events ADD CONSTRAINT events_half_price_mode_check
    CHECK (half_price_mode IN ('quota', 'linked'));

-- A trilha ganha a entidade 'event': é onde a escolha da cota fica registrada, com valor,
-- data e usuário. Mesma tabela — duas seriam dois lugares para procurar quando alguém
-- contesta, e duas chances de esquecer de gravar.
ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS audit_events_entity_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_entity_check
    CHECK (entity IN ('refund_request', 'ticket', 'courtesy', 'export', 'event'));

-- event_id na trilha: sem ele, a linha da cota de meia não tem em que se pendurar.
ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS event_id uuid;
CREATE INDEX IF NOT EXISTS audit_events_event_idx ON audit_events (event_id, at)
    WHERE event_id IS NOT NULL;
