package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestReviewRestrictedToCheckin: só quem fez check-in avalia, uma vez.
func TestReviewRestrictedToCheckin(t *testing.T) {
	ts, pool, _ := setupSigned(t)
	_, owner := createProducer(t, ts, "Casa Confianca", "owner@confianca.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	tokens := soldSeatedTickets(t, ts, pool, owner, pid, 1)
	tid := firstTicket(t, ctx, pool, pid)

	// Admissão gera a presença (com checkin_id).
	if _, vb := do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner), map[string]any{"token": tokens[0], "gate": "G1"}); vb["verdict"] != "admitted" {
		t.Fatalf("admissão: %v", vb)
	}
	var checkinID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT checkin_id FROM attendance_records WHERE ticket_id=$1`, tid).Scan(&checkinID); err != nil {
		t.Fatalf("checkin_id: %v", err)
	}

	// Check-in fabricado não avalia.
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkins/"+uuid.NewString()+"/review", nil,
		map[string]any{"rating": 5}); code != http.StatusForbidden {
		t.Fatalf("review sem check-in: esperava 403, veio %d", code)
	}
	// Com check-in válido → ok.
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkins/"+checkinID.String()+"/review", nil,
		map[string]any{"rating": 5, "body": "Ótimo!"}); code != http.StatusCreated {
		t.Fatalf("review válido: esperava 201, veio %d", code)
	}
	// Segunda avaliação do mesmo check-in → 409.
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkins/"+checkinID.String()+"/review", nil,
		map[string]any{"rating": 3}); code != http.StatusConflict {
		t.Fatalf("review duplicado: esperava 409, veio %d", code)
	}

	// Reputação verificável: marca o evento como entregue e confere.
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `UPDATE events SET status='finished'`); err != nil {
			t.Fatalf("finalizar evento: %v", err)
		}
	})
	code, body := do(t, ts, "GET", "/api/v1/public/producers/"+pid.String()+"/reputation", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("reputação: %d", code)
	}
	if body["rating_avg"].(float64) != 5 {
		t.Fatalf("média esperada 5, veio %v", body["rating_avg"])
	}
	rep, _ := body["reputation"].(map[string]any)
	if rep["events_delivered"].(float64) != 1 {
		t.Fatalf("eventos entregues esperado 1, veio %v", rep["events_delivered"])
	}
}

// TestDiscoveryByPresence: sugestões vêm de quem esteve nos mesmos lugares.
func TestDiscoveryByPresence(t *testing.T) {
	ts, pool := setup(t)
	pidStr, owner := createProducer(t, ts, "Casa Descoberta", "owner@descoberta.com", "senha1234")
	pid := producerID(t, ts, owner)
	_ = owner
	ctx := context.Background()

	// Dois sujeitos; A e B foram ao mesmo evento1; B também foi ao evento2.
	var sA, sB uuid.UUID
	_ = pool.QueryRow(ctx, `INSERT INTO subjects DEFAULT VALUES RETURNING id`).Scan(&sA)
	_ = pool.QueryRow(ctx, `INSERT INTO subjects DEFAULT VALUES RETURNING id`).Scan(&sB)
	ev1, ev2 := uuid.New(), uuid.New()
	att := func(subject, event uuid.UUID, title string) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO attendance_records (subject_id, producer_id, event_id, event_title)
			VALUES ($1,$2,$3,$4)`, subject, pid, event, title); err != nil {
			t.Fatalf("attendance: %v", err)
		}
	}
	att(sA, ev1, "Evento 1")
	att(sB, ev1, "Evento 1")
	att(sB, ev2, "Evento 2")

	code, body := do(t, ts, "GET", "/api/v1/public/subjects/"+sA.String()+"/discovery", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("descoberta: %d", code)
	}
	sug, _ := body["suggestions"].([]any)
	if len(sug) != 1 {
		t.Fatalf("esperava 1 sugestão, veio %v", body["suggestions"])
	}
	s0, _ := sug[0].(map[string]any)
	if s0["event_title"] != "Evento 2" || s0["producer"] != "Casa Descoberta" {
		t.Fatalf("sugestão inesperada: %v", s0)
	}
	_ = pidStr
}
