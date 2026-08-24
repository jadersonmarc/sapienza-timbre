-- Ficha do participante por ingresso. O ingresso é nominal: quem compra quatro entradas
-- informa quem vai usar cada uma, e é esse nome que a portaria confere com o documento.
-- Sem isso, quatro ingressos iguais não têm dono distinguível e a entrada vira honra.
ALTER TABLE tickets
    ADD COLUMN IF NOT EXISTS attendee_name  text,
    ADD COLUMN IF NOT EXISTS attendee_cpf   text,
    ADD COLUMN IF NOT EXISTS attendee_email text;

-- A ficha é coletada no checkout mas só vira ingresso quando o pagamento confirma (o
-- webhook cria os tickets), então ela viaja na ordem até lá.
ALTER TABLE orders
    ADD COLUMN IF NOT EXISTS attendees jsonb NOT NULL DEFAULT '[]'::jsonb;

CREATE INDEX IF NOT EXISTS tickets_attendee_cpf_idx ON tickets (attendee_cpf) WHERE attendee_cpf IS NOT NULL;
