-- Camada pública (Onda 1): cidade do evento, para o filtro de cidade no diretório público.
-- events já tem address/lat/lng, mas não uma cidade discreta para agrupar/filtrar. Forward.
ALTER TABLE events ADD COLUMN IF NOT EXISTS city text;
