package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
)

// TestReemissaoInvalidaOQRAnterior é a regra que impede fraude servida pelo painel: reemitir
// sem queimar o anterior deixaria dois QR válidos para o mesmo lugar.
func TestReemissaoInvalidaOQRAnterior(t *testing.T) {
	ts, pool, signer := setupSigned(t)
	_, owner := createProducer(t, ts, "Casa Reemissao", "owner@reemissao.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	_ = signer
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 1, "buy@reemissao.com", "pix")

	tickets := ticketsOf(t, ctx, pool, pid, eventID)
	antigo := tickets[0]
	var tokenAntigo string
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		tok, err := ticketing.TicketToken(ctx, tx, uuid.MustParse(antigo))
		if err != nil {
			t.Fatalf("token: %v", err)
		}
		tokenAntigo = tok
	})

	// Estado do lote e do assento ANTES: a reemissão é a mesma venda, não pode mexer neles.
	soldAntes := scanInt(t, ctx, pool, pid, `SELECT sold_count FROM lots WHERE event_id=$1`, eventID)
	razaoAntes := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM ledger_entries`)

	code, body := do(t, ts, "POST", "/api/v1/tickets/"+antigo+"/reissue", bearer(owner),
		map[string]any{"reason": "QR não abria"})
	if code != http.StatusCreated {
		t.Fatalf("reemissão: %d %v", code, body)
	}
	novo, _ := body["ticket_id"].(string)
	if novo == "" || novo == antigo {
		t.Fatalf("esperava um ingresso novo, veio %v", body)
	}

	// O QR antigo morre: a portaria recusa.
	_, v := do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner),
		map[string]any{"token": tokenAntigo, "gate": "G1"})
	if verdict(v) == "admitted" {
		t.Fatalf("o QR anterior não pode continuar entrando, veredito %s", verdict(v))
	}
	// O novo entra.
	var tokenNovo string
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		tok, err := ticketing.TicketToken(ctx, tx, uuid.MustParse(novo))
		if err != nil {
			t.Fatalf("token novo: %v", err)
		}
		tokenNovo = tok
	})
	_, v = do(t, ts, "POST", "/api/v1/gate/validate", bearer(owner),
		map[string]any{"token": tokenNovo, "gate": "G1"})
	if verdict(v) != "admitted" {
		t.Fatalf("o ingresso reemitido precisa entrar, veredito %s (%v)", verdict(v), v)
	}

	// Nada de estoque, lote, assento ou razão mudou: é a MESMA venda.
	if n := scanInt(t, ctx, pool, pid, `SELECT sold_count FROM lots WHERE event_id=$1`, eventID); n != soldAntes {
		t.Fatalf("reemissão não pode mexer no lote: %d → %d", soldAntes, n)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM ledger_entries`); n != razaoAntes {
		t.Fatalf("reemissão não pode mexer no razão: %d → %d", razaoAntes, n)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM seat_occupancy WHERE NOT released`); n != 1 {
		t.Fatalf("o assento continua ocupado, por UM ingresso; veio %d", n)
	}

	// A trilha registra quem fez e por quê.
	code, hist := do(t, ts, "GET", "/api/v1/tickets/"+antigo+"/history", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("histórico: %d", code)
	}
	evs, _ := hist["events"].([]any)
	if len(evs) != 1 {
		t.Fatalf("esperava 1 evento na trilha, veio %v", hist["events"])
	}
	e0, _ := evs[0].(map[string]any)
	if e0["to_status"] != "reissued" || e0["reason"] != "QR não abria" {
		t.Fatalf("trilha incompleta: %v", e0)
	}
}

// TestReemissaoParaEndereçoNovo: o e-mail digitado errado é o caso mais comum. O novo passa a
// valer no pedido, e o anterior fica na trilha — sem ele, ninguém explica para onde o
// primeiro ingresso foi.
func TestReemissaoParaEnderecoNovo(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Email", "owner@emailerrado.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 1, "errado@x.com", "pix")
	tickets := ticketsOf(t, ctx, pool, pid, eventID)

	code, body := do(t, ts, "POST", "/api/v1/tickets/"+tickets[0]+"/reissue", bearer(owner),
		map[string]any{"to_email": "certo@x.com", "reason": "e-mail digitado errado"})
	if code != http.StatusCreated {
		t.Fatalf("reemissão: %d %v", code, body)
	}
	if body["delivered_to"] != "certo@x.com" {
		t.Fatalf("esperava entrega no endereço novo, veio %v", body["delivered_to"])
	}
	if s := scanStr(t, ctx, pool, pid, `SELECT buyer_email FROM orders WHERE event_id=$1`, eventID); s != "certo@x.com" {
		t.Fatalf("o pedido precisa passar a valer com o e-mail novo, veio %s", s)
	}
	_, hist := do(t, ts, "GET", "/api/v1/tickets/"+tickets[0]+"/history", bearer(owner), nil)
	evs, _ := hist["events"].([]any)
	d, _ := evs[0].(map[string]any)["details"].(map[string]any)
	if d["email_anterior"] != "errado@x.com" {
		t.Fatalf("o endereço anterior precisa ficar na trilha, veio %v", d)
	}
}

// TestAcoesBloqueadasComEntradaRegistrada: quem passou na portaria está lá dentro. Reemitir
// cria QR novo para quem já usou o antigo; transferir muda o nome de quem já foi conferido.
func TestAcoesBloqueadasComEntradaRegistrada(t *testing.T) {
	ts, pool := setup(t)
	pidStr, owner := createProducer(t, ts, "Casa Entrada", "owner@entrada.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 1, "buy@entrada.com", "pix")
	tickets := ticketsOf(t, ctx, pool, pid, eventID)
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `INSERT INTO checkins (ticket_id, is_reentry) VALUES ($1,false)`, tickets[0]); err != nil {
			t.Fatalf("registrar entrada: %v", err)
		}
	})

	for _, acao := range []string{"reissue", "transfer-to"} {
		if code, _ := do(t, ts, "POST", "/api/v1/tickets/"+tickets[0]+"/"+acao, bearer(owner),
			map[string]any{"to_email": "outro@x.com", "reason": "tentativa"}); code != http.StatusConflict {
			t.Fatalf("%s com entrada registrada: esperava 409, veio %d", acao, code)
		}
	}

	// O admin passa, com motivo.
	admin := seedAdmin(t, ts, pool, "admin@entrada.com", "admin")
	if code, _ := do(t, ts, "POST", "/api/v1/admin/producers/"+pidStr+"/tickets/"+tickets[0]+"/reissue",
		admin, nil); code != http.StatusBadRequest {
		t.Fatalf("admin sem motivo: esperava 400, veio %d", code)
	}
	if code, b := do(t, ts, "POST", "/api/v1/admin/producers/"+pidStr+"/tickets/"+tickets[0]+"/reissue",
		admin, map[string]any{"reason": "aparelho da portaria falhou"}); code != http.StatusCreated {
		t.Fatalf("admin com motivo: %d %v", code, b)
	}
}

// TestTransferenciaPeloProdutor: troca de titular não é revenda — não gera cobrança, não
// mexe em split nem em royalty, e os dois lados são avisados.
func TestTransferenciaPeloProdutor(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Titular", "owner@titular.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 1, "antigo@x.com", "pix")
	tickets := ticketsOf(t, ctx, pool, pid, eventID)

	pagamentosAntes := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM payments`)
	royaltyAntes := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM royalty_entries`)

	code, body := do(t, ts, "POST", "/api/v1/tickets/"+tickets[0]+"/transfer-to", bearer(owner),
		map[string]any{"to_email": "novo@x.com", "reason": "comprou no nome do amigo"})
	if code != http.StatusOK {
		t.Fatalf("transferência: %d %v", code, body)
	}

	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM payments`); n != pagamentosAntes {
		t.Fatalf("troca de titular não pode gerar cobrança")
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM royalty_entries WHERE amount_cents > 0`); n != royaltyAntes {
		t.Fatalf("troca de titular não é revenda: nada de royalty")
	}
	// O índice público passa ao novo titular — é dele que o ingresso aparece agora.
	var email string
	if err := pool.QueryRow(ctx, `SELECT buyer_email FROM ticket_directory WHERE ticket_id=$1`,
		uuid.MustParse(tickets[0])).Scan(&email); err != nil {
		t.Fatalf("diretório: %v", err)
	}
	if email != "novo@x.com" {
		t.Fatalf("esperava o novo titular no diretório, veio %s", email)
	}
	// O titular anterior é avisado de que perdeu o ingresso.
	if n := countNotifications(t, ctx, pool, "waitlist"); n < 1 {
		t.Fatalf("o titular anterior precisa ser avisado")
	}
	_ = eventID
}
