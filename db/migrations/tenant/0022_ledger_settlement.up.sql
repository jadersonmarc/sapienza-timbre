-- Liquidação no razão: separar o que a plataforma DEVE do que ela apenas registrou.
--
-- Duas contas passaram a mentir quando o split entrou em uso, e as duas moram aqui.

-- ── razão ─────────────────────────────────────────────────────────────────────
-- 'estorno_taxa' separa o que volta ao COMPRADOR pela plataforma (conveniência) do que
-- volta pelo produtor (face). Antes existia um lançamento só, com o total do comprador, e
-- ele descontava do produtor os 10% que nunca foram dele.
ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_entries_kind_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_kind_check
    CHECK (kind IN ('repasse', 'retencao', 'conciliacao', 'estorno', 'estorno_taxa', 'taxa', 'processamento'));

-- settled_by diz QUEM entregou o dinheiro ao produtor. Com o split, o face vai direto para
-- a subconta dele na própria cobrança — o repasse existe como receita, mas a plataforma não
-- deve mais nada. Sem esta coluna, ledger.NetDue soma como dívida um valor que o gateway já
-- pagou, e a fila de repasse do /admin manda pagar duas vezes.
--
-- É propriedade da VENDA, não da linha: todas as linhas de um pedido carregam o mesmo
-- valor. A retenção de 5% também depende dela — reter só faz sentido sobre dinheiro que a
-- plataforma está segurando, e no split ela não está segurando nada.
ALTER TABLE ledger_entries ADD COLUMN IF NOT EXISTS settled_by text NOT NULL DEFAULT 'platform';
ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_entries_settled_by_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_settled_by_check
    CHECK (settled_by IN ('platform', 'split'));

-- Lançamentos existentes de venda que passou pelo split são retroativamente marcados: o
-- dinheiro foi entregue pelo gateway, não pela plataforma.
UPDATE ledger_entries le
   SET settled_by = 'split'
  FROM split_transfers st
 WHERE le.order_id = st.order_id
   AND st.asaas_payment_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS ledger_entries_kind_idx ON ledger_entries (kind, order_id);
