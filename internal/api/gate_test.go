package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
)

// soldSeatedTickets cria um evento com mapa, vende `n` assentos em Pix, confirma e
// devolve os tokens dos ingressos assinados (em ordem).
func soldSeatedTickets(t *testing.T, ts *httptest.Server, pool *pgxpool.Pool, owner string, pid uuid.UUID, n int) []string {
	t.Helper()
	ctx := context.Background()
	eventID, seats, lotID := seatedEvent(t, ts, pool, owner, pid, n)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar: %d", code)
	}
	seatStrs := make([]string, n)
	for i, s := range seats {
		seatStrs[i] = s.String()
	}
	_, body := do(t, ts, "POST", "/api/v1/public/checkout", nil, map[string]any{
		"event_id": eventID.String(), "lot_id": lotID.String(), "quantity": n,
		"seat_ids": seatStrs, "method": "pix",
	})
	asaasRef, _ := body["asaas_ref"].(string)
	confirmWebhook(t, ts, asaasRef)

	var tokens []string
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		rows, err := tx.Query(ctx, `SELECT id FROM tickets WHERE seat_id IS NOT NULL ORDER BY created_at`)
		if err != nil {
			t.Fatalf("listar tickets: %v", err)
		}
		var ids []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				t.Fatalf("scan: %v", err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			tok, err := ticketing.TicketToken(ctx, tx, id)
			if err != nil {
				t.Fatalf("token: %v", err)
			}
			tokens = append(tokens, tok)
		}
	})
	return tokens
}

func verdict(body map[string]any) string {
	v, _ := body["verdict"].(string)
	return v
}

// TestGateValidateAdmitDuplicateReentry cobre o veredito da portaria online.
func TestGateValidateAdmitDuplicateReentry(t *testing.T) {
	ts, pool, signer := setupSigned(t)
	_, owner := createProducer(t, ts, "Casa Portaria", "owner@portaria.com", "senha1234")
	pid := producerID(t, ts, owner)
	tokens := soldSeatedTickets(t, ts, pool, owner, pid, 1)
	tok := tokens[0]

	// Config traz a chave pública que a portaria embarca.
	code, cfg := do(t, ts, "GET", "/api/v1/gate/config", bearer(owner), nil)
	if code != http.StatusOK || cfg["public_key"] != signer.PublicKeyB64() {
		t.Fatalf("config: %d, %v", code, cfg)
	}

	// Primeira entrada: admitido, com direcionamento de assento.
	code, body := do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner), map[string]any{"token": tok, "gate": "G1"})
	if code != http.StatusOK || verdict(body) != "admitted" {
		t.Fatalf("admissão: %d, %v", code, body)
	}
	seat, _ := body["seat"].(map[string]any)
	if seat["sector"] != "Plateia" || seat["row"] != "A" {
		t.Fatalf("esperava setor/fileira no veredito, veio %v", seat)
	}

	// Segunda leitura sem reentrada: duplicata.
	_, body = do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner), map[string]any{"token": tok, "gate": "G1"})
	if verdict(body) != "duplicate" {
		t.Fatalf("esperava duplicate, veio %v", body)
	}
	// Com reentrada explícita: reentry.
	_, body = do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner), map[string]any{"token": tok, "gate": "G1", "reentry": true})
	if verdict(body) != "reentry" {
		t.Fatalf("esperava reentry, veio %v", body)
	}

	// Token forjado: inválido.
	bad := []byte(tok)
	bad[len(bad)/2] ^= 0x01
	_, body = do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner), map[string]any{"token": string(bad), "gate": "G1"})
	if verdict(body) != "invalid" {
		t.Fatalf("esperava invalid, veio %v", body)
	}
}

// TestGateSyncReconciles: dois dispositivos escaneiam o mesmo ingresso offline; a
// reconciliação admite um e marca o outro como duplicata. client_uid é idempotente.
func TestGateSyncReconciles(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Sync", "owner@sync.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	tokens := soldSeatedTickets(t, ts, pool, owner, pid, 1)
	tok := tokens[0]

	sync := func(clientUID, gateName string) string {
		_, body := do(t, ts, "POST", "/api/v1/gate/sync", bearer(owner), map[string]any{
			"checkins": []map[string]any{{"token": tok, "gate": gateName, "device_id": clientUID, "client_uid": clientUID}},
		})
		results, _ := body["results"].([]any)
		if len(results) != 1 {
			t.Fatalf("esperava 1 resultado, veio %v", body)
		}
		r, _ := results[0].(map[string]any)
		v, _ := r["verdict"].(string)
		return v
	}

	if v := sync("u1", "G1"); v != "admitted" {
		t.Fatalf("device 1: esperava admitted, veio %s", v)
	}
	if v := sync("u2", "G2"); v != "duplicate" {
		t.Fatalf("device 2 (mesmo ingresso): esperava duplicate, veio %s", v)
	}
	// Reenvio do mesmo scan (u1) é idempotente.
	if v := sync("u1", "G1"); v != "admitted" {
		t.Fatalf("reenvio u1: esperava admitted (idempotente), veio %s", v)
	}
	// Exatamente uma admissão primária no banco.
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM checkins WHERE NOT is_reentry`); n != 1 {
		t.Fatalf("esperava 1 admissão primária, veio %d", n)
	}
}

// TestGateRequiresCheckinPermission: quem não tem 'checkin' não valida.
func TestGateRequiresCheckinPermission(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducer(t, ts, "Casa Perm2", "owner@perm2.com", "senha1234")
	if code, _ := do(t, ts, "POST", "/api/v1/collaborators", bearer(owner),
		map[string]any{"email": "rel@perm2.com", "password": "senha1234", "permissions": []string{"relatorios"}}); code != http.StatusCreated {
		t.Fatalf("criar colaborador: %d", code)
	}
	rel := login(t, ts, "rel@perm2.com", "senha1234")
	if code, _ := do(t, ts, "POST", "/api/v1/gate/validate", bearer(rel), map[string]any{"token": "x"}); code != http.StatusForbidden {
		t.Fatalf("sem checkin: esperava 403, veio %d", code)
	}
}
