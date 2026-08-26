-- Subconta do produtor no gateway. A conta é do PRODUTOR, não do evento: um produtor tem
-- uma subconta, reusada em todos os eventos dele. Criar por evento repetiria o CPF/CNPJ e o
-- gateway recusa a segunda criação.
--
-- O ciclo de vida dela é independente do evento. O produtor monta o evento enquanto a
-- análise corre; só ABRIR VENDA exige conta aprovada.
CREATE TABLE IF NOT EXISTS producer_asaas_accounts (
    producer_id  uuid PRIMARY KEY REFERENCES producers(id) ON DELETE CASCADE,
    -- Um documento, uma subconta. Sócios da mesma produtora com o mesmo CNPJ compartilham
    -- a subconta em vez de tentarem criar duas — o gateway recusaria a segunda.
    cpf_cnpj     text NOT NULL UNIQUE,
    person_type  text NOT NULL CHECK (person_type IN ('PF','PJ')),
    wallet_id    text NOT NULL,
    -- sem_conta é o estado implícito (linha ausente); os demais acompanham a análise.
    account_status text NOT NULL DEFAULT 'criada_aguardando_docs'
        CHECK (account_status IN ('criada_aguardando_docs','em_analise','aprovada','reprovada')),
    status_reason  text,
    -- Confirmação anual de dados comerciais: exigência regulatória. Vencida, a subconta
    -- perde o uso da API — a data vem do gateway e é atualizada por webhook, nunca por
    -- polling (ela só muda uma vez por ano).
    commercial_info_expires_at timestamptz,
    onboarding_url text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Não existe coluna para a apiKey da subconta: ela volta uma única vez na criação e é
-- descartada de propósito. O split não precisa dela, e custodiar credencial de terceiro é
-- responsabilidade sem contrapartida.

-- Produtores que já tinham carteira informada à mão entram como aprovados: a carteira só
-- existe ali porque alguém já a validou por fora.
INSERT INTO producer_asaas_accounts (producer_id, cpf_cnpj, person_type, wallet_id, account_status)
SELECT p.id, 'legado:' || p.id::text, 'PJ', p.asaas_wallet_id, 'aprovada'
  FROM producers p
 WHERE p.asaas_wallet_id IS NOT NULL AND p.asaas_wallet_id <> ''
ON CONFLICT (producer_id) DO NOTHING;

-- Pendências de documentação da subconta, para reexpor o link quando um documento é
-- reprovado e um novo é gerado.
CREATE TABLE IF NOT EXISTS producer_asaas_documents (
    producer_id    uuid NOT NULL REFERENCES producers(id) ON DELETE CASCADE,
    document_id    text NOT NULL,
    document_type  text,
    status         text,
    onboarding_url text,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (producer_id, document_id)
);
