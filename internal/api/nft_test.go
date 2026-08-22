package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestTokenMetadataNoPersonalData: os metadados públicos ERC-1155 têm o evento, mas NUNCA
// dado pessoal do comprador.
func TestTokenMetadataNoPersonalData(t *testing.T) {
	ts, pool, _ := setupSigned(t)
	_, owner := createProducer(t, ts, "Casa Token", "owner@token.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()

	eventID := createEvent(t, ts, owner, "Show Token", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 100, 0)
	do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil)
	body := buyViaSession(t, ts, buyer(t, ts, pool, "secreto@x.com"), map[string]any{
		"event_id": eventID, "quantity": 1,
	}, "pix")
	confirmWebhook(t, ts, body["asaas_ref"].(string))
	tid := firstTicket(t, ctx, pool, pid)

	code, meta := rawGet(t, ts, "/api/v1/public/tokens/"+tid.String()+"/metadata", "")
	if code != http.StatusOK {
		t.Fatalf("metadata: %d", code)
	}
	if !strings.Contains(meta, "Show Token") {
		t.Fatalf("metadata deveria ter o evento: %s", meta)
	}
	if strings.Contains(meta, "secreto@x.com") || strings.Contains(strings.ToLower(meta), "buyer") {
		t.Fatalf("metadata NÃO pode ter dado pessoal: %s", meta)
	}

	// Estado do token (Pix → transferível; sem rede → aguardando emissão).
	code, sv := do(t, ts, "GET", "/api/v1/public/tokens/"+tid.String(), nil, nil)
	if code != http.StatusOK {
		t.Fatalf("token view: %d", code)
	}
	state, _ := sv["state"].(map[string]any)
	if state["lifecycle"] != "transferivel" {
		t.Fatalf("estado inesperado: %v", state)
	}
}

// TestDisputeBlocksTransferNotEntry: disputa bloqueia a transferência mas não a entrada.
func TestDisputeBlocksTransferNotEntry(t *testing.T) {
	ts, pool, _ := setupSigned(t)
	_, owner := createProducer(t, ts, "Casa Disputa", "owner@disputa.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	tokens := soldSeatedTickets(t, ts, pool, owner, pid, 1)
	tid := firstTicket(t, ctx, pool, pid)

	if code, _ := do(t, ts, "POST", "/api/v1/tickets/"+tid.String()+"/dispute", bearer(owner),
		map[string]any{"reason": "contestação"}); code != http.StatusOK {
		t.Fatalf("abrir disputa: %d", code)
	}
	// Transferência bloqueada.
	wallet := mkWallet(t, pool, "0xdisputa")
	if code, _ := do(t, ts, "POST", "/api/v1/tickets/"+tid.String()+"/transfer", bearer(owner),
		map[string]any{"to_wallet_id": wallet.String(), "price_cents": 0}); code != http.StatusConflict {
		t.Fatalf("transferir em disputa: esperava 409, veio %d", code)
	}
	// Entrada continua funcionando.
	if _, vb := do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner), map[string]any{"token": tokens[0], "gate": "G1"}); vb["verdict"] != "admitted" {
		t.Fatalf("entrada em disputa: esperava admitted, veio %v", vb)
	}
}

// TestReissue: reemissão queima o antigo e emite um novo assinado.
func TestReissue(t *testing.T) {
	ts, pool, _ := setupSigned(t)
	_, owner := createProducer(t, ts, "Casa Reemissao", "owner@reemissao.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	tokens := soldSeatedTickets(t, ts, pool, owner, pid, 1)
	oldID := firstTicket(t, ctx, pool, pid)

	code, body := do(t, ts, "POST", "/api/v1/tickets/"+oldID.String()+"/reissue", bearer(owner), nil)
	if code != http.StatusCreated {
		t.Fatalf("reemissão: %d %v", code, body)
	}
	newIDStr, _ := body["ticket_id"].(string)
	newID := uuid.MustParse(newIDStr)

	if st := scanStr(t, ctx, pool, pid, `SELECT status FROM tickets WHERE id=$1`, oldID); st != "burned" {
		t.Fatalf("antigo deveria estar queimado, veio %s", st)
	}
	// Novo ativo e assinado.
	var status string
	var signed bool
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT status, signature IS NOT NULL AND qr_nonce IS NOT NULL FROM tickets WHERE id=$1`, newID).Scan(&status, &signed); err != nil {
			t.Fatalf("novo ingresso: %v", err)
		}
	})
	if status != "active" || !signed {
		t.Fatalf("novo ingresso deveria estar ativo e assinado (status=%s signed=%v)", status, signed)
	}
	// O QR antigo não vale mais; o novo vale.
	if _, vb := do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner), map[string]any{"token": tokens[0], "gate": "G1"}); vb["verdict"] != "invalid" {
		t.Fatalf("QR antigo (queimado): esperava invalid, veio %v", vb)
	}
	newTok := tokenOf(t, ctx, pool, pid, newID)
	if _, vb := do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner), map[string]any{"token": newTok, "gate": "G1"}); vb["verdict"] != "admitted" {
		t.Fatalf("QR novo: esperava admitted, veio %v", vb)
	}
}
