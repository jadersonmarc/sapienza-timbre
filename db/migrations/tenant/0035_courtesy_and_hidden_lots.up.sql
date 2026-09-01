-- Cortesia por formulário e categoria com link oculto.

-- ── cortesia: contato do destinatário ─────────────────────────────────────────
-- O convidado era só nome e CPF: não havia para onde mandar o ingresso, e o produtor
-- reenviava por WhatsApp na mão. Com e-mail, a emissão entrega.
--
-- É dado pessoal de TERCEIRO entrando no sistema pela mão do produtor. Por isso o aviso
-- que o destinatário recebe identifica QUEM emitiu: quem recebe um e-mail com o próprio
-- nome precisa saber de onde ele veio.
ALTER TABLE guest_list_entries ADD COLUMN IF NOT EXISTS email text;
ALTER TABLE guest_list_entries ADD COLUMN IF NOT EXISTS phone text;

-- ── categoria com link oculto ─────────────────────────────────────────────────
-- Categoria que não aparece na página pública e só é alcançada por um link exclusivo:
-- pré-venda de lista, setor de convidados, lote de parceiro.
ALTER TABLE lots ADD COLUMN IF NOT EXISTS hidden boolean NOT NULL DEFAULT false;

-- O link. O token é gerado por crypto/rand e guardado inteiro: não é sequencial, não é
-- derivado do id do lote e não dá para chutar a partir de outro.
--
-- Revogar é gravar revoked_at, e a checagem é feita a cada uso — link revogado para de
-- funcionar na hora, não na próxima virada de cache.
CREATE TABLE IF NOT EXISTS lot_links (
    id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    lot_id  uuid NOT NULL REFERENCES lots(id) ON DELETE CASCADE,
    token   text NOT NULL,
    label   text,

    -- Nulo = sem limite. O uso é contado na COMPRA, não na visita: abrir o link para ver o
    -- preço não pode gastar a vaga de ninguém.
    max_uses   int CHECK (max_uses IS NULL OR max_uses > 0),
    used_count int NOT NULL DEFAULT 0,

    expires_at timestamptz,
    revoked_at timestamptz,

    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS lot_links_token_key ON lot_links (token);
CREATE INDEX IF NOT EXISTS lot_links_lot_idx ON lot_links (lot_id);

-- Qual link trouxe o pedido. O uso do link é contado quando o pagamento CONFIRMA, não
-- quando a sessão é criada: um Pix aberto e abandonado não pode gastar a vaga de ninguém.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS lot_link_id uuid REFERENCES lot_links(id);
