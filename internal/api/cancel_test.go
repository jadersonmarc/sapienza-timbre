package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/api"
	"github.com/jadersonmarc/sapienza-timbre/internal/chain"
)

// TestCancelamentoDevolveTodoMundo é o "pronto quando" da fatia: cancelar um evento devolve
// o dinheiro de todos os compradores e invalida os ingressos.
//
// Antes, cancelar era só mudar o status — o evento sumia do diretório e cada comprador
// ficava com ingresso válido e dinheiro pago, descobrindo sozinho.
func TestCancelamentoDevolveTodoMundo(t *testing.T) {
	ts, pool, srv := setupWithServer(t)
	_, owner := createProducer(t, ts, "Casa Cancelada", "owner@cancelada.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()

	// Um evento com três compras de pessoas diferentes.
	eventID, seats, _ := seatedEvent(t, ts, pool, owner, pid, 3)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar")
	}
	for i, seat := range seats {
		email := []string{"a@cancel.com", "b@cancel.com", "c@cancel.com"}[i]
		body := buyViaSession(t, ts, buyer(t, ts, pool, email), map[string]any{
			"event_id": eventID.String(), "quantity": 1, "seat_ids": []string{seat.String()},
		}, "pix")
		confirmWebhook(t, ts, body["asaas_ref"].(string))
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='active'`); n != 3 {
		t.Fatalf("esperava 3 ingressos ativos antes do cancelamento, veio %d", n)
	}

	// Cancelar ENFILEIRA — não devolve na requisição. Mil ingressos seriam mil chamadas ao
	// gateway com o produtor olhando para a tela travada.
	code, body := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/cancel", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("cancelar: %d %v", code, body)
	}
	if body["refunds_queued"] != float64(3) {
		t.Fatalf("esperava 3 devoluções enfileiradas, veio %v", body["refunds_queued"])
	}
	// Nada devolvido ainda, e os ingressos ainda não foram queimados.
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='active'`); n != 3 {
		t.Fatalf("a devolução não pode acontecer na requisição do cancelamento")
	}
	// Mas todo mundo já foi avisado: quem tinha ingresso para amanhã precisa saber hoje.
	if n := countNotifications(t, ctx, pool, "event_cancelled"); n != 3 {
		t.Fatalf("esperava 3 avisos de cancelamento, veio %d", n)
	}

	// O worker devolve.
	w := api.NewCancelWorker(pool, srv)
	if err := w.ProcessTenant(ctx, pid); err != nil {
		t.Fatalf("worker: %v", err)
	}

	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='burned'`); n != 3 {
		t.Fatalf("esperava 3 ingressos queimados, veio %d", n)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM orders WHERE status='refunded'`); n != 3 {
		t.Fatalf("esperava 3 pedidos estornados, veio %d", n)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM refund_jobs WHERE status='done'`); n != 3 {
		t.Fatalf("esperava 3 devoluções concluídas, veio %d", n)
	}
	// A capacidade volta ao lote e os assentos ficam livres — o evento cancelado não pode
	// deixar o inventário travado.
	if n := scanInt(t, ctx, pool, pid, `SELECT sold_count FROM lots WHERE event_id=$1`, eventID); n != 0 {
		t.Fatalf("esperava capacidade devolvida, sold_count=%d", n)
	}

	// Progresso, que é o que o painel mostra.
	code, prog := do(t, ts, "GET", "/api/v1/events/"+eventID.String()+"/cancellation", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("progresso: %d", code)
	}
	if prog["total"] != float64(3) || prog["done"] != float64(3) || prog["pending"] != float64(0) {
		t.Fatalf("progresso inesperado: %v", prog)
	}
}

// TestCancelamentoNaoDuplicaDevolucao: cancelar duas vezes (dois cliques, ou um retry) não
// pode virar duas devoluções para o mesmo dinheiro.
func TestCancelamentoNaoDuplicaDevolucao(t *testing.T) {
	ts, pool, srv := setupWithServer(t)
	_, owner := createProducer(t, ts, "Casa Duplo Cancel", "owner@duplocancel.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 1, "buy@duplocancel.com", "pix")

	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/cancel", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("primeiro cancelamento")
	}
	// Segunda chamada: a transição é idempotente e nada novo entra na fila.
	code, body := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/cancel", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("segundo cancelamento: %d %v", code, body)
	}
	if body["refunds_queued"] != float64(0) {
		t.Fatalf("o segundo cancelamento não pode enfileirar de novo, veio %v", body["refunds_queued"])
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM refund_jobs`); n != 1 {
		t.Fatalf("esperava 1 devolução na fila, veio %d", n)
	}

	w := api.NewCancelWorker(pool, srv)
	for range 2 {
		if err := w.ProcessTenant(ctx, pid); err != nil {
			t.Fatalf("worker: %v", err)
		}
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM refunds`); n != 1 {
		t.Fatalf("esperava 1 estorno executado, veio %d", n)
	}
}

// TestCancelamentoFalhaViraTrabalhoManual: esgotadas as tentativas, a devolução aparece na
// fila de resolução manual do admin, com o motivo, e pode ser reenfileirada depois que
// alguém resolveu a causa.
func TestCancelamentoFalhaViraTrabalhoManual(t *testing.T) {
	ts, pool, _, gw, srv := setupAll(t, chain.NoopChainDriver{}, chain.MintModeEager)
	_, owner := createProducer(t, ts, "Casa Falha", "owner@falha.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, ref := soldEvent(t, ts, pool, owner, pid, 1, "buy@falha.com", "pix")

	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/cancel", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("cancelar")
	}

	w := api.NewCancelWorker(pool, srv)
	w.MaxAttempts = 1 // esgota na primeira, para o teste não depender de backoff
	gw.FailRefund(ref, errGatewayFora)
	if err := w.ProcessTenant(ctx, pid); err != nil {
		t.Fatalf("worker: %v", err)
	}
	if s := scanStr(t, ctx, pool, pid, `SELECT status FROM refund_jobs LIMIT 1`); s != "failed" {
		t.Fatalf("esperava devolução falha, veio %s", s)
	}
	// O ingresso NÃO foi queimado: o dinheiro não voltou.
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='active'`); n != 1 {
		t.Fatalf("sem devolução, o ingresso não pode ser queimado")
	}

	// A falha aparece para a plataforma, com o motivo.
	admin := seedAdmin(t, ts, pool, "admin@falha.com", "admin")
	code, body := do(t, ts, "GET", "/api/v1/admin/refund-jobs/failed", admin, nil)
	if code != http.StatusOK {
		t.Fatalf("fila de falhas: %d %v", code, body)
	}
	failed, _ := body["failed"].([]any)
	if len(failed) != 1 {
		t.Fatalf("esperava 1 devolução na fila manual, veio %v", body["failed"])
	}
	row, _ := failed[0].(map[string]any)
	if row["last_error"] == "" || row["last_error"] == nil {
		t.Fatalf("a falha precisa dizer o motivo: %v", row)
	}

	// Resolvida a causa, o admin reenfileira e o worker conclui.
	jobID, _ := row["job_id"].(string)
	if code, _ := do(t, ts, "POST", "/api/v1/admin/producers/"+pid.String()+"/refund-jobs/"+jobID+"/retry", admin, nil); code != http.StatusOK {
		t.Fatalf("reenfileirar: %d", code)
	}
	if err := w.ProcessTenant(ctx, pid); err != nil {
		t.Fatalf("worker (segunda volta): %v", err)
	}
	if s := scanStr(t, ctx, pool, pid, `SELECT status FROM refund_jobs LIMIT 1`); s != "done" {
		t.Fatalf("esperava devolução concluída, veio %s", s)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE status='burned'`); n != 1 {
		t.Fatalf("esperava o ingresso queimado, veio %d", n)
	}
}

// TestCancelamentoRepublicaAtestadoUmaVez: evento fechado que é cancelado tem o registro
// canônico republicado UMA vez, ao fim do lote. Republicar a cada devolução geraria uma
// versão por pedido, e o atestado viraria ruído em vez de prova.
func TestCancelamentoRepublicaAtestadoUmaVez(t *testing.T) {
	ts, pool, _, _, srv := setupAttestFull(t, chain.NoopAnchorer{}, chain.AnchorModeOff)
	_, owner := createProducer(t, ts, "Casa Atestado Cancel", "owner@atcancel.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID, _, _ := soldEvent(t, ts, pool, owner, pid, 2, "buy@atcancel.com", "pix")

	if code, b := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/close", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("fechar: %d %v", code, b)
	}
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID.String()+"/cancel", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("cancelar")
	}
	// Enquanto o lote corre, o atestado NÃO é republicado: seria uma versão por pedido.
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM event_attestations WHERE event_id=$1`, eventID); n != 1 {
		t.Fatalf("antes do worker deveria haver 1 atestado, veio %d", n)
	}

	w := api.NewCancelWorker(pool, srv)
	if err := w.ProcessTenant(ctx, pid); err != nil {
		t.Fatalf("worker: %v", err)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM event_attestations WHERE event_id=$1`, eventID); n != 2 {
		t.Fatalf("esperava 2 versões (a original e a corrigida), veio %d", n)
	}
	// Rodar de novo não gera uma terceira: o lote já terminou e a marca foi limpa.
	if err := w.ProcessTenant(ctx, pid); err != nil {
		t.Fatalf("worker (segunda volta): %v", err)
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM event_attestations WHERE event_id=$1`, eventID); n != 2 {
		t.Fatalf("republicação repetida: esperava 2 versões, veio %d", n)
	}
}

func countNotifications(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, kind string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE kind=$1`, kind).Scan(&n); err != nil {
		t.Fatalf("contar notificações: %v", err)
	}
	return n
}

var errGatewayFora = errGateway("gateway indisponível")

type errGateway string

func (e errGateway) Error() string { return string(e) }
