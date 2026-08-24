package api_test

import (
	"net/http"
	"testing"
)

// TestBuyerTransferMovesOwnership (Onda 2): o comprador transfere o ingresso para outro
// e-mail; a posse migra no ticket_directory — o remetente deixa de ver, o destinatário
// passa a ver após entrar com o e-mail de destino.
func TestBuyerTransferMovesOwnership(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducer(t, ts, "CasaTransf", "owner@transf.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show Transf", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publish: %d", code)
	}
	// Ana entra ANTES de comprar (a conta é exigida no pagamento, pela sessão).
	ana := buyerToken(t, ts, "ana@x.com")
	res := buyViaSession(t, ts, bearer(ana), map[string]any{"event_id": eventID, "quantity": 1}, "pix")
	confirmWebhook(t, ts, res["asaas_ref"].(string))

	// Ana vê o ingresso.
	_, mine := do(t, ts, "GET", "/api/v1/public/me/tickets", bearer(ana), nil)
	tickets := asSlice(mine["tickets"])
	if len(tickets) != 1 {
		t.Fatalf("Ana deveria ter 1 ingresso, veio %v", tickets)
	}
	ticketID := tickets[0].(map[string]any)["ticket_id"].(string)

	// Ana transfere para o e-mail do Bruno.
	if code, tb := do(t, ts, "POST", "/api/v1/public/me/tickets/"+ticketID+"/transfer", bearer(ana),
		map[string]any{"to_email": "bruno@x.com"}); code != http.StatusOK {
		t.Fatalf("transferência: %d %v", code, tb)
	}

	// Ana não vê mais; Bruno vê após entrar.
	_, anaAfter := do(t, ts, "GET", "/api/v1/public/me/tickets", bearer(ana), nil)
	if len(asSlice(anaAfter["tickets"])) != 0 {
		t.Fatalf("após transferir, Ana não deveria ver o ingresso: %v", anaAfter["tickets"])
	}
	bruno := buyerToken(t, ts, "bruno@x.com")
	_, brunoMine := do(t, ts, "GET", "/api/v1/public/me/tickets", bearer(bruno), nil)
	if len(asSlice(brunoMine["tickets"])) != 1 {
		t.Fatalf("Bruno deveria ver o ingresso transferido, veio %v", brunoMine["tickets"])
	}
}

// TestBuyerReissue (§3.2): o comprador reemite o próprio ingresso; sai um ticket_id novo e
// o anterior é queimado.
func TestBuyerReissue(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducer(t, ts, "CasaReissue", "owner@reissue.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show Reissue", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publish: %d", code)
	}
	clara := buyerToken(t, ts, "clara@x.com")
	res := buyViaSession(t, ts, bearer(clara), map[string]any{"event_id": eventID, "quantity": 1}, "pix")
	confirmWebhook(t, ts, res["asaas_ref"].(string))
	_, mine := do(t, ts, "GET", "/api/v1/public/me/tickets", bearer(clara), nil)
	oldID := asSlice(mine["tickets"])[0].(map[string]any)["ticket_id"].(string)

	code, rr := do(t, ts, "POST", "/api/v1/public/me/tickets/"+oldID+"/reissue", bearer(clara), nil)
	if code != http.StatusOK {
		t.Fatalf("reissue: %d %v", code, rr)
	}
	newID := rr["ticket_id"].(string)
	if newID == oldID || newID == "" {
		t.Fatalf("reemissão deveria gerar novo ticket_id, veio %q", newID)
	}
	// "Meus ingressos" passa a mostrar o novo; o antigo consta queimado no tenant.
	_, after := do(t, ts, "GET", "/api/v1/public/me/tickets", bearer(clara), nil)
	got := asSlice(after["tickets"])
	if len(got) != 1 || got[0].(map[string]any)["ticket_id"] != newID {
		t.Fatalf("me/tickets deveria mostrar o novo ingresso, veio %v", got)
	}
}

// TestBuyerTransferRejectsNonOwner: não-dono não transfere ingresso alheio (403).
func TestBuyerTransferRejectsNonOwner(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducer(t, ts, "CasaTransf2", "owner@transf2.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show Transf2", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publish: %d", code)
	}
	dona := buyerToken(t, ts, "dona@x.com")
	res := buyViaSession(t, ts, bearer(dona), map[string]any{"event_id": eventID, "quantity": 1}, "pix")
	confirmWebhook(t, ts, res["asaas_ref"].(string))
	_, mine := do(t, ts, "GET", "/api/v1/public/me/tickets", bearer(dona), nil)
	ticketID := asSlice(mine["tickets"])[0].(map[string]any)["ticket_id"].(string)

	// Estranho (outro subject) tenta transferir o ingresso da Dona.
	estranho := buyerToken(t, ts, "estranho@x.com")
	if code, _ := do(t, ts, "POST", "/api/v1/public/me/tickets/"+ticketID+"/transfer", bearer(estranho),
		map[string]any{"to_email": "ladrao@x.com"}); code != http.StatusForbidden {
		t.Fatalf("não-dono transferindo: esperava 403, veio %d", code)
	}
}
