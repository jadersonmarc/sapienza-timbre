-- Subtítulo do evento: a linha curta abaixo do título, na página e no card de
-- compartilhamento. "Turnê de despedida", "com participação de X" — o que faz alguém
-- entender o evento sem ler a descrição inteira.
--
-- Curto por CHECK, não por convenção: subtítulo que vira parágrafo deixa de ser subtítulo, e
-- quebra o layout da página e o corte do card social.
ALTER TABLE events ADD COLUMN IF NOT EXISTS subtitle text;
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_subtitle_len_chk;
ALTER TABLE events ADD CONSTRAINT events_subtitle_len_chk
    CHECK (subtitle IS NULL OR length(subtitle) <= 160);

-- A descrição existe desde a fundação, mas nunca teve teto. Texto de terceiro sem limite é
-- página que não carrega e e-mail que não entrega.
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_description_len_chk;
ALTER TABLE events ADD CONSTRAINT events_description_len_chk
    CHECK (description IS NULL OR length(description) <= 5000);
