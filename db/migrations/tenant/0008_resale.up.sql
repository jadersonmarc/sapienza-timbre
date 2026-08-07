-- Transferência restrita e royalty (Etapa 2.1). Estes valores espelham as constantes
-- IMUTÁVEIS do contrato da plataforma (teto de revenda e royalty embutidos em código);
-- a garantia real vem da transferência restrita on-chain — aqui é o espelho off-chain
-- que valida antes de enfileirar.

-- Teto de preço de revenda, em % do valor de face (100 = não pode passar do face).
ALTER TABLE events ADD COLUMN IF NOT EXISTS resale_cap_pct numeric(5,2) NOT NULL DEFAULT 100;
-- Royalty da revenda, em % do preço da transferência.
ALTER TABLE events ADD COLUMN IF NOT EXISTS royalty_pct numeric(5,2) NOT NULL DEFAULT 10;
