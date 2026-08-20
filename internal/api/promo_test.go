package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestCampaignAttributionAndAudience: campanha com UTM, clique e atribuição da compra,
// refletida no perfil do público por fonte; pixels do evento públicos.
func TestCampaignAttributionAndAudience(t *testing.T) {
	ts, pool, _ := setupSigned(t)
	_, owner := createProducer(t, ts, "Casa Divulga", "owner@divulga.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()

	eventID := createEvent(t, ts, owner, "Show Divulga", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil)
	_, lots := getEventLots(t, ts, owner, eventID)

	// Campanha instagram.
	code, cb := do(t, ts, "POST", "/api/v1/events/"+eventID+"/campaigns", bearer(owner),
		map[string]any{"name": "Insta lançamento", "utm_source": "instagram"})
	if code != http.StatusCreated {
		t.Fatalf("criar campanha: %d %v", code, cb)
	}
	campaignID, _ := cb["id"].(string)

	// Clique no link parametrizado (público).
	if code, _ := do(t, ts, "POST", "/api/v1/public/campaigns/"+campaignID+"/click", nil, nil); code != http.StatusOK {
		t.Fatalf("click: %d", code)
	}
	_, lb := do(t, ts, "GET", "/api/v1/events/"+eventID+"/campaigns", bearer(owner), nil)
	camps, _ := lb["campaigns"].([]any)
	c0, _ := camps[0].(map[string]any)
	if c0["clicks"].(float64) != 1 {
		t.Fatalf("esperava 1 clique, veio %v", c0["clicks"])
	}

	// Compra atribuída à campanha.
	_, body := do(t, ts, "POST", "/api/v1/public/checkout", buyer(t, ts, pool, "buy@promo.com"), map[string]any{
		"event_id": eventID, "lot_id": lots[0], "quantity": 1, "method": "pix", "campaign_id": campaignID,
	})
	confirmWebhook(t, ts, body["asaas_ref"].(string))

	// Perfil por fonte mostra instagram.
	code, ab := do(t, ts, "GET", "/api/v1/dash/events/"+eventID+"/audience", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("audiência: %d", code)
	}
	sources, _ := ab["by_source"].([]any)
	found := false
	for _, s := range sources {
		m, _ := s.(map[string]any)
		if m["source"] == "instagram" && m["orders"].(float64) == 1 && m["tickets"].(float64) == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("perfil do público não atribuiu ao instagram: %v", ab["by_source"])
	}

	// Pixels do evento (públicos).
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `UPDATE events SET meta_pixel='PIXEL123' WHERE id=$1`, eventID); err != nil {
			t.Fatalf("pixel: %v", err)
		}
	})
	_, px := do(t, ts, "GET", "/api/v1/public/events/"+eventID+"/pixels", nil, nil)
	if px["meta_pixel"] != "PIXEL123" {
		t.Fatalf("pixel esperado, veio %v", px)
	}
}

// TestWaitlistNotifiedOnRollover: a lista de espera é avisada quando o lote vira (esgota).
func TestWaitlistNotifiedOnRollover(t *testing.T) {
	ts, pool, _ := setupSigned(t)
	_, owner := createProducer(t, ts, "Casa Espera", "owner@espera.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()

	eventID := createEvent(t, ts, owner, "Show Espera", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 1, 0) // estoque 1 → esgota na 1ª venda
	do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil)
	_, lots := getEventLots(t, ts, owner, eventID)

	// Alguém entra na lista de espera.
	if code, _ := do(t, ts, "POST", "/api/v1/public/events/"+eventID+"/waitlist", nil,
		map[string]any{"email": "fila@x.com"}); code != http.StatusCreated {
		t.Fatalf("waitlist: %d", code)
	}

	// Uma compra esgota o lote → virada → aviso.
	_, body := do(t, ts, "POST", "/api/v1/public/checkout", buyer(t, ts, pool, "buy@waitlist.com"), map[string]any{
		"event_id": eventID, "lot_id": lots[0], "quantity": 1, "method": "pix",
	})
	confirmWebhook(t, ts, body["asaas_ref"].(string))

	if notified := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM waitlist WHERE event_id=$1 AND notified_at IS NOT NULL`, uuid.MustParse(eventID)); notified != 1 {
		t.Fatalf("esperava 1 inscrito avisado na virada, veio %d", notified)
	}
}
