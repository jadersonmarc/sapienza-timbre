package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/jadersonmarc/sapienza-timbre/internal/chain"
)

// TestSecondaryMarketResale é o "pronto quando" da Etapa 2.2: um anúncio é comprado, o
// pagamento confirma, a titularidade passa com segurança (royalty + taxa registrados) e
// a procedência mostra a cadeia de posse.
func TestSecondaryMarketResale(t *testing.T) {
	ts, pool, _ := setupCore(t, okChain{})
	_, owner := createProducer(t, ts, "Casa Revenda", "owner@revenda.com", "senha1234")
	pid := producerID(t, ts, owner)
	setRetention(t, pool, pid, 10)
	ctx := context.Background()
	soldStandingTicket(t, ts, owner) // Pix: transferível já; face 5000
	tid := firstTicket(t, ctx, pool, pid)

	// Teto no anúncio: 6000 > 5000 → recusa.
	if code, _ := do(t, ts, "POST", "/api/v1/tickets/"+tid.String()+"/listings", bearer(owner),
		map[string]any{"price_cents": 6000}); code != http.StatusBadRequest {
		t.Fatalf("anúncio acima do teto: esperava 400, veio %d", code)
	}

	// Anuncia a 4000.
	code, lbody := do(t, ts, "POST", "/api/v1/tickets/"+tid.String()+"/listings", bearer(owner),
		map[string]any{"price_cents": 4000})
	if code != http.StatusCreated {
		t.Fatalf("anúncio: %d %v", code, lbody)
	}
	listingID, _ := lbody["id"].(string)

	// Compra PÚBLICA (sem conta).
	code, bbody := do(t, ts, "POST", "/api/v1/public/listings/"+listingID+"/buy", nil,
		map[string]any{"buyer_email": "comprador2@x.com"})
	if code != http.StatusCreated {
		t.Fatalf("compra: %d %v", code, bbody)
	}
	asaasRef, _ := bbody["asaas_ref"].(string)

	// Antes da confirmação: titularidade ainda não mudou.
	if o := scanStr(t, ctx, pool, pid, `SELECT COALESCE(owner_wallet_id::text,'') FROM tickets WHERE id=$1`, tid); o != "" {
		t.Fatalf("titularidade não deveria ter mudado ainda, veio %s", o)
	}

	// Webhook confirma a revenda.
	confirmWebhook(t, ts, asaasRef)

	// Titularidade passou, anúncio vendido, royalty + taxa registrados.
	if o := scanStr(t, ctx, pool, pid, `SELECT COALESCE(owner_wallet_id::text,'') FROM tickets WHERE id=$1`, tid); o == "" {
		t.Fatal("titularidade deveria ter passado ao comprador")
	}
	if st := scanStr(t, ctx, pool, pid, `SELECT status FROM listings WHERE ticket_id=$1`, tid); st != "sold" {
		t.Fatalf("anúncio deveria estar sold, veio %s", st)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM royalty_entries WHERE amount_cents=400`); n != 1 {
		t.Fatalf("esperava royalty de 400 (10%% de 4000), veio %d", n)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM ledger_entries WHERE kind='taxa' AND amount_cents=400`); n != 1 {
		t.Fatalf("esperava taxa de revenda 400, veio %d", n)
	}

	// Procedência: cadeia de posse com o elo da revenda.
	code, prov := do(t, ts, "GET", "/api/v1/tickets/"+tid.String()+"/provenance", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("procedência: %d", code)
	}
	if prov["face_cents"].(float64) != 5000 {
		t.Fatalf("valor de face esperado 5000, veio %v", prov["face_cents"])
	}
	links, _ := prov["chain"].([]any)
	if len(links) != 1 {
		t.Fatalf("esperava 1 elo na cadeia, veio %v", prov["chain"])
	}
	link0, _ := links[0].(map[string]any)
	if link0["price_cents"].(float64) != 4000 {
		t.Fatalf("elo com preço 4000 esperado, veio %v", link0)
	}

	// Registro on-chain assíncrono da transferência confirma.
	if _, err := chain.ProcessTenant(ctx, pool, okChain{}, pid); err != nil {
		t.Fatalf("process: %v", err)
	}
	if st := scanStr(t, ctx, pool, pid, `SELECT status FROM transfers WHERE ticket_id=$1`, tid); st != "confirmed" {
		t.Fatalf("transfer on-chain deveria confirmar, veio %s", st)
	}
}
