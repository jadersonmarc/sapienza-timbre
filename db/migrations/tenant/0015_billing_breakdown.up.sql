-- Modelo de cobrança "Sympla" (§4): a taxa é do COMPRADOR, separada do valor de face.
-- orders.total_cents passa a ser o TOTAL cobrado (face + taxa de conveniência); guardamos a
-- decomposição para o razão ter três linhas (repasse do face, taxa de plataforma, custo de
-- processamento repassado à adquirência) e para o estorno saber o que devolver.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS face_cents           bigint NOT NULL DEFAULT 0;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS platform_fee_cents   bigint NOT NULL DEFAULT 0;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS processing_fee_cents bigint NOT NULL DEFAULT 0;

-- Nova linha do razão: o custo de processamento repassado à adquirência.
ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_entries_kind_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_kind_check
  CHECK (kind IN ('repasse','retencao','conciliacao','estorno','taxa','processamento'));
