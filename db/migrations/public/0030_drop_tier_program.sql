-- O programa de níveis foi extinto: a taxa é 10% do face para todo produtor, num ponto só.
--
-- O código já não consultava nada disto — mas as tabelas continuavam de pé, `producers.tier`
-- continuava sendo devolvido pela API e o painel administrativo ainda escrevia "Nível
-- iniciante" ao lado do nome de cada produtor. Estado parado é lido como estado real pela
-- próxima pessoa que olhar.
DROP TABLE IF EXISTS producer_tier_history;
DROP TABLE IF EXISTS platform_fee_rules;
ALTER TABLE producers DROP COLUMN IF EXISTS tier;

-- A ORIGINAÇÃO continua: ela não é nível nem rebate, é participação de quem indicou sobre a
-- fatia da PLATAFORMA. Fica inerte enquanto participation_pct for 0.
