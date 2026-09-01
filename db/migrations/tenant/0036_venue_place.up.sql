-- Local do evento por busca (Google Places) — sem perder a inserção manual.
--
-- events já tem address/city/lat/lng. Falta o NOME do estabelecimento (que é o que o
-- comprador procura, não o CEP) e o identificador do lugar.
ALTER TABLE events ADD COLUMN IF NOT EXISTS venue_name text;

-- place_id é o identificador estável do lugar. Guardamos ELE e as coordenadas — que os
-- termos do Google permitem reter — e NÃO cacheamos o endereço formatado por tempo
-- indeterminado. O endereço que fica gravado é o que o produtor confirmou no formulário,
-- editável por ele: é dado do evento, não cópia do catálogo de terceiro.
ALTER TABLE events ADD COLUMN IF NOT EXISTS place_id text;
