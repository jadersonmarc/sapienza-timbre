-- Lotes flexíveis: progressivo, simultâneo e categoria avulsa no mesmo evento.
--
-- O modelo já resolvia a virada progressiva (o primeiro lote elegível por ordem, data e
-- capacidade). O que faltava era poder tirar um tipo de ingresso DA FILA — "Camarote" e
-- "Pista" não são lote 1 e lote 2, são coisas diferentes vendidas ao mesmo tempo.

-- Como o lote entra na oferta:
--   sequential — participa da fila de virada: só o primeiro elegível é oferecido
--   always     — é oferecido por conta própria, sempre que estiver na janela e com estoque
--
-- 'always' cobre os dois casos do produto — lotes SIMULTÂNEOS e a CATEGORIA AVULSA (que é um
-- 'always' sem datas). A diferença entre eles é de rótulo na tela, não de mecanismo, e
-- inventar dois valores para o mesmo comportamento só criaria um estado a manter em sincronia.
ALTER TABLE lots ADD COLUMN IF NOT EXISTS availability text NOT NULL DEFAULT 'sequential';
ALTER TABLE lots DROP CONSTRAINT IF EXISTS lots_availability_check;
ALTER TABLE lots ADD CONSTRAINT lots_availability_check
    CHECK (availability IN ('sequential', 'always'));

-- O que ENCERRA um lote da fila — e, portanto, o que abre o próximo:
--   either  — o que ocorrer primeiro: esgotar ou chegar a data (comportamento histórico)
--   sellout — só o esgotamento; a data de fim é ignorada para encerrar
--   date    — só a data; esgotar ANTES não adianta a virada, e o evento fica sem nada à
--             venda até lá. É a escolha de quem promete "o lote 2 abre no dia X".
ALTER TABLE lots ADD COLUMN IF NOT EXISTS turn_trigger text NOT NULL DEFAULT 'either';
ALTER TABLE lots DROP CONSTRAINT IF EXISTS lots_turn_trigger_check;
ALTER TABLE lots ADD CONSTRAINT lots_turn_trigger_check
    CHECK (turn_trigger IN ('either', 'sellout', 'date'));
