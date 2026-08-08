-- Passe de temporada (Etapa 2.3): a compra de um passe emite UM ingresso por data, e
-- cada ingresso é independente — destacável e repassável individualmente (é um ticket
-- normal, então reusa transferência restrita e mercado secundário).

-- Lote que o ingresso de cada data do passe ocupa (para o ticket ter lot_id válido).
ALTER TABLE season_pass_dates ADD COLUMN IF NOT EXISTS lot_id uuid REFERENCES lots(id);

-- Vínculo do ingresso ao passe que o originou (informativo/rastreio).
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS season_pass_id uuid REFERENCES season_passes(id);

-- Ordem de compra do passe (a ordem não é de um evento único, e sim do passe).
ALTER TABLE orders ADD COLUMN IF NOT EXISTS season_pass_id uuid REFERENCES season_passes(id);
