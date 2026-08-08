package api_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestAttendanceOnCheckin é o "pronto quando" da Etapa 2.4: a admissão gera um registro
// de presença (intransferível, gratuito); reentrada não gera outro.
func TestAttendanceOnCheckin(t *testing.T) {
	ts, pool, _ := setupSigned(t)
	_, owner := createProducer(t, ts, "Casa Presenca", "owner@presenca.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	tokens := soldSeatedTickets(t, ts, pool, owner, pid, 1)
	tok := tokens[0]
	tid := firstTicket(t, ctx, pool, pid)

	// Admissão → presença registrada.
	if _, vb := do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner), map[string]any{"token": tok, "gate": "G1"}); vb["verdict"] != "admitted" {
		t.Fatalf("admissão: %v", vb)
	}
	if n := countAttendance(t, ctx, pool, tid); n != 1 {
		t.Fatalf("esperava 1 registro de presença, veio %d", n)
	}

	// Vínculo ao produtor.
	var producerID2 uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT producer_id FROM attendance_records WHERE ticket_id=$1`, tid).Scan(&producerID2); err != nil {
		t.Fatalf("attendance: %v", err)
	}
	if producerID2 != pid {
		t.Fatalf("presença deveria vincular ao produtor %s, veio %s", pid, producerID2)
	}

	// Reentrada não gera novo registro.
	if _, vb := do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner), map[string]any{"token": tok, "gate": "G1", "reentry": true}); vb["verdict"] != "reentry" {
		t.Fatalf("reentrada: %v", vb)
	}
	if n := countAttendance(t, ctx, pool, tid); n != 1 {
		t.Fatalf("reentrada não deveria gerar novo registro, veio %d", n)
	}

	// Intransferível por desenho: a tabela não tem coluna de transferência.
	var cols int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema='public' AND table_name='attendance_records'
		   AND column_name ILIKE '%transfer%'`).Scan(&cols); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if cols != 0 {
		t.Fatalf("attendance_records não deveria ter coluna de transferência, achou %d", cols)
	}
}

func countAttendance(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ticketID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM attendance_records WHERE ticket_id=$1`, ticketID).Scan(&n); err != nil {
		t.Fatalf("count attendance: %v", err)
	}
	return n
}
