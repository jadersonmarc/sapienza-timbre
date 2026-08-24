-- Conta do comprador no padrão comercial: cadastro com dados reais e senha própria. O
-- código por e-mail deixa de ser a porta de entrada e vira verificação de endereço e
-- recuperação de senha.
--
-- Os campos não são enfeite de formulário: nome e CPF são exigidos pelo gateway para criar
-- o cliente da cobrança (hoje toda cobrança nasce chamada "Comprador" e sem documento), o
-- telefone é o contato no dia do evento e a data de nascimento sustenta meia-entrada por
-- idade.
ALTER TABLE subjects
    ADD COLUMN IF NOT EXISTS phone             text,
    ADD COLUMN IF NOT EXISTS birth_date        date,
    ADD COLUMN IF NOT EXISTS password_hash     text,
    ADD COLUMN IF NOT EXISTS email_verified_at timestamptz,
    ADD COLUMN IF NOT EXISTS updated_at        timestamptz NOT NULL DEFAULT now();

-- Uma conta por e-mail. Antes de exigir isso, consolida o que já existe: contas duplicadas
-- nasciam do resolveSubjectByEmail (select-then-insert sem unicidade), e criar o índice sem
-- tratá-las derrubaria o boot em produção. As referências das duplicatas são repontadas para
-- a conta mais antiga percorrendo as chaves estrangeiras do catálogo — assim nenhuma tabela
-- futura fica esquecida aqui.
DO $$
DECLARE
    fk    record;
    total bigint;
BEGIN
    CREATE TEMP TABLE subject_merge ON COMMIT DROP AS
    SELECT s.id AS dup_id, k.keep_id
      FROM subjects s
      JOIN (
            SELECT lower(email) AS em, (array_agg(id ORDER BY created_at))[1] AS keep_id
              FROM subjects WHERE email IS NOT NULL
             GROUP BY 1 HAVING count(*) > 1
           ) k ON lower(s.email) = k.em AND s.id <> k.keep_id;

    SELECT count(*) INTO total FROM subject_merge;
    IF total = 0 THEN
        RETURN;
    END IF;

    FOR fk IN
        SELECT c.conrelid::regclass::text AS tbl, a.attname AS col
          FROM pg_constraint c
          JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
         WHERE c.contype = 'f' AND c.confrelid = 'subjects'::regclass
    LOOP
        EXECUTE format(
            'UPDATE %s t SET %I = m.keep_id FROM subject_merge m WHERE t.%I = m.dup_id',
            fk.tbl, fk.col, fk.col);
    END LOOP;

    -- notifications guarda uma referência SOLTA (sem FK), então não aparece no laço acima.
    UPDATE notifications n SET subject_id = m.keep_id FROM subject_merge m WHERE n.subject_id = m.dup_id;

    DELETE FROM subjects WHERE id IN (SELECT dup_id FROM subject_merge);
    RAISE NOTICE 'subjects: % contas duplicadas consolidadas por e-mail', total;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS subjects_email_uniq
    ON subjects (lower(email)) WHERE email IS NOT NULL;
