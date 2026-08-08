-- Panorama de passeios (Etapa 2.5). Snapshot do evento no registro de presença, para
-- montar o mapa/linha do tempo cross-produtor sem varrer os schemas dos produtores (o
-- evento vive no schema do produtor; a presença é pública).
ALTER TABLE attendance_records ADD COLUMN IF NOT EXISTS event_title text;
ALTER TABLE attendance_records ADD COLUMN IF NOT EXISTS venue_lat double precision;
ALTER TABLE attendance_records ADD COLUMN IF NOT EXISTS venue_lng double precision;
