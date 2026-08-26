-- Última tabela de tarifas conhecida do gateway. O preço do ingresso depende dela, então
-- ela precisa sobreviver a uma falha de rede no meio de uma venda: sem o último valor
-- persistido, ou o checkout trava ou passa a calcular com tarifa inventada.
--
-- Linha única (id fixo): não há histórico aqui — o histórico que importa é o snapshot
-- gravado em cada venda, que é o que explica um preço antigo numa auditoria.
CREATE TABLE IF NOT EXISTS asaas_fee_table (
    id         int PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    fees       jsonb NOT NULL,
    raw        jsonb,
    fetched_at timestamptz NOT NULL DEFAULT now()
);
