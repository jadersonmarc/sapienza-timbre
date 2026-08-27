-- Combo (duplo, trio, grupo) é QUANTIDADE MÍNIMA DE COMPRA, não ingresso especial.
--
-- Um "ingresso duplo" é um lote com mínimo 2 e máximo 2. O preço cadastrado continua sendo o
-- UNITÁRIO e o comprador paga preço × quantidade. Trio, quarteto e combo de grupo saem da
-- mesma regra, sem código novo.
--
-- A compra gera N ingressos INDEPENDENTES, cada um com seu QR, transferíveis e estornáveis
-- separadamente: nada muda na portaria, no atestado ou na contagem de público. Um ingresso
-- continua sendo uma pessoa — por isso não existe campo de "quantas pessoas o ingresso
-- admite", que mudaria o significado de tudo o que conta gente.
ALTER TABLE lots ADD COLUMN IF NOT EXISTS min_purchase_quantity int NOT NULL DEFAULT 1;
ALTER TABLE lots ADD COLUMN IF NOT EXISTS max_purchase_quantity int;

ALTER TABLE lots DROP CONSTRAINT IF EXISTS lots_purchase_range_chk;
ALTER TABLE lots ADD CONSTRAINT lots_purchase_range_chk CHECK (
    min_purchase_quantity >= 1
    AND (max_purchase_quantity IS NULL OR max_purchase_quantity >= min_purchase_quantity)
);
