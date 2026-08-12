package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestAudienceConsentReachNoIdentity cobre a Fase 3: segmentação por presença, alcance só
// com consentimento (granular/revogável), recompensa a quem autoriza, e métricas do
// patrocinador SEM identidade.
func TestAudienceConsentReachNoIdentity(t *testing.T) {
	ts, pool := setup(t)
	pidStr, _ := createProducer(t, ts, "Casa Audiencia", "owner@audiencia.com", "senha1234")
	pid := uuid.MustParse(pidStr)
	ctx := context.Background()
	admin := map[string]string{"X-Admin-Token": adminToken}

	// Dois sujeitos: A com 2 presenças, B com 1.
	var sA, sB uuid.UUID
	_ = pool.QueryRow(ctx, `INSERT INTO subjects DEFAULT VALUES RETURNING id`).Scan(&sA)
	_ = pool.QueryRow(ctx, `INSERT INTO subjects DEFAULT VALUES RETURNING id`).Scan(&sB)
	att := func(subject uuid.UUID) {
		if _, err := pool.Exec(ctx, `INSERT INTO attendance_records (subject_id, producer_id, event_id) VALUES ($1,$2,$3)`, subject, pid, uuid.New()); err != nil {
			t.Fatalf("attendance: %v", err)
		}
	}
	att(sA)
	att(sA)
	att(sB)

	// Segmento por presença: quem tem >= 2 presenças.
	code, seg := do(t, ts, "POST", "/api/v1/admin/segments", admin,
		map[string]any{"name": "Frequentadores", "definition": map[string]any{"min_attendances": 2}})
	if code != http.StatusCreated {
		t.Fatalf("criar segmento: %d %v", code, seg)
	}
	segID, _ := seg["id"].(string)

	// Recompute dimensiona o segmento (só A).
	code, rc := do(t, ts, "POST", "/api/v1/admin/segments/"+segID+"/recompute", admin, nil)
	if code != http.StatusOK || rc["size"].(float64) != 1 {
		t.Fatalf("recompute: %d %v", code, rc)
	}

	// A consente (granular). Gera recompensa escalada pela circulação (2 presenças → 200).
	if code, _ := do(t, ts, "POST", "/api/v1/public/subjects/"+sA.String()+"/consents", nil,
		map[string]any{"segment_id": segID, "granted": true}); code != http.StatusOK {
		t.Fatalf("consentir: %d", code)
	}
	var reward int64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(cr.amount),0) FROM consent_rewards cr
		  JOIN consents c ON c.id=cr.consent_id WHERE c.subject_id=$1`, sA).Scan(&reward); err != nil {
		t.Fatalf("reward: %v", err)
	}
	if reward != 200 {
		t.Fatalf("recompensa esperada 200 (2 presenças), veio %d", reward)
	}

	// Patrocinador: campanha sobre o segmento; entrega só a quem consentiu.
	code, camp := do(t, ts, "POST", "/api/v1/admin/sponsor-campaigns", admin,
		map[string]any{"sponsor": "Marca X", "segment_id": segID, "budget": 1000})
	if code != http.StatusCreated {
		t.Fatalf("campanha: %d %v", code, camp)
	}
	campID, _ := camp["id"].(string)
	code, dl := do(t, ts, "POST", "/api/v1/admin/sponsor-campaigns/"+campID+"/deliver", admin, nil)
	if code != http.StatusOK || dl["delivered"].(float64) != 1 {
		t.Fatalf("entrega: %d %v", code, dl)
	}

	// Métricas SEM identidade: só números, e nada que identifique A ou B.
	code, m := do(t, ts, "GET", "/api/v1/admin/sponsor-campaigns/"+campID+"/metrics", admin, nil)
	if code != http.StatusOK {
		t.Fatalf("métricas: %d", code)
	}
	if m["segment_size"].(float64) != 1 || m["consented_size"].(float64) != 1 || m["delivered"].(float64) != 1 {
		t.Fatalf("métricas: %v", m)
	}
	_, raw := do(t, ts, "GET", "/api/v1/admin/sponsor-campaigns/"+campID+"/metrics", admin, nil)
	if s := toStr(raw); strings.Contains(s, sA.String()) || strings.Contains(s, sB.String()) {
		t.Fatalf("métricas NÃO podem conter identidade: %s", s)
	}

	// Revogar tira do alcance na hora.
	if code, _ := do(t, ts, "POST", "/api/v1/public/subjects/"+sA.String()+"/consents", nil,
		map[string]any{"segment_id": segID, "granted": false}); code != http.StatusOK {
		t.Fatalf("revogar: %d", code)
	}
	_, m2 := do(t, ts, "GET", "/api/v1/admin/sponsor-campaigns/"+campID+"/metrics", admin, nil)
	if m2["consented_size"].(float64) != 0 {
		t.Fatalf("após revogar, alcance consentido deveria ser 0, veio %v", m2["consented_size"])
	}
}

func toStr(m map[string]any) string {
	var b strings.Builder
	for k, v := range m {
		b.WriteString(k)
		b.WriteString(":")
		if s, ok := v.(string); ok {
			b.WriteString(s)
		}
		b.WriteString(" ")
	}
	return b.String()
}
