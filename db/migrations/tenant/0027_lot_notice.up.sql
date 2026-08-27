-- Aviso do produtor por CATEGORIA de ingresso (aqui, o lote), não por evento.
--
-- "Acomodações por ordem de chegada" só faz sentido no lote sem assento marcado; preso ao
-- evento, apareceria também para quem comprou lugar numerado e diria o contrário do que a
-- pessoa acabou de escolher.
--
-- Guardado como TEXTO PURO. O conteúdo é de terceiro e vai para página pública e e-mail:
-- HTML aqui seria injeção com endereço de entrega. A sanitização é na escrita, não na
-- leitura — o que está no banco já é seguro para qualquer superfície.
ALTER TABLE lots ADD COLUMN IF NOT EXISTS notice text;
ALTER TABLE lots DROP CONSTRAINT IF EXISTS lots_notice_len_chk;
ALTER TABLE lots ADD CONSTRAINT lots_notice_len_chk CHECK (notice IS NULL OR length(notice) <= 280);
