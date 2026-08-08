-- Registro de presença (Etapa 2.4). Uma presença por ingresso (a admissão primária é
-- única por ingresso na portaria; este índice reforça a idempotência do registro).
-- attendance_records NÃO tem coluna de transferência de propósito: presença é
-- intransferível por AUSÊNCIA de mecanismo, não por regra de aplicação.
CREATE UNIQUE INDEX IF NOT EXISTS attendance_records_ticket_key
    ON attendance_records (ticket_id) WHERE ticket_id IS NOT NULL;
