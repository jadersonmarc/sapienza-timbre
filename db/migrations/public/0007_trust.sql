-- Descoberta e confiança (Etapa 2.6). Uma avaliação por check-in: garante que só quem
-- entrou avalia, e uma vez só. reviews.checkin_id já é NOT NULL (avaliação restrita a
-- quem fez check-in é do schema).
CREATE UNIQUE INDEX IF NOT EXISTS reviews_checkin_key ON reviews (checkin_id);
