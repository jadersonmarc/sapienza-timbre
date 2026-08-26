package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestConcurrentAccountCreationCreatesOne: duplo clique ou dois eventos publicados quase
// juntos disparam duas criações. O unique constraint sozinho não bastaria — a chamada ao
// gateway acontece antes do insert —, então o advisory lock precisa segurar a segunda.
func TestConcurrentAccountCreationCreatesOne(t *testing.T) {
	ts, pool := setup(t)
	pid, owner := createProducerWithoutWallet(t, ts, "Casa Corrida", "owner@corrida.com", "senha1234")
	body := map[string]any{
		"legal_name": "Casa Corrida Produções", "tax_id": testCPF("corrida@x.com"),
		"email": "financeiro@corrida.com", "mobile_phone": "31988887777", "birth_date": "1985-03-10",
		"income_cents": 500000, "postal_code": "30140-071", "address": "Rua dos Aimorés",
		"address_number": "100", "province": "Funcionários",
	}

	var wg sync.WaitGroup
	carteiras := make([]string, 4)
	for i := range carteiras {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, resp := do(t, ts, "POST", "/api/v1/producer/receiving-account", bearer(owner), body)
			if code == http.StatusOK {
				carteiras[i], _ = resp["wallet_id"].(string)
			}
		}(i)
	}
	wg.Wait()

	var contas int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM producer_asaas_accounts WHERE producer_id=$1`, uuid.MustParse(pid)).Scan(&contas); err != nil {
		t.Fatalf("contar contas: %v", err)
	}
	if contas != 1 {
		t.Fatalf("esperava exatamente uma conta para o produtor, veio %d", contas)
	}
	// Todas as respostas bem-sucedidas devolvem a MESMA carteira.
	var primeira string
	for _, w := range carteiras {
		if w == "" {
			continue
		}
		if primeira == "" {
			primeira = w
		} else if w != primeira {
			t.Fatalf("criações concorrentes devolveram carteiras diferentes: %q e %q", primeira, w)
		}
	}
}

// TestSplitTransferRecordedWithFeeSnapshot: a venda registra o repasse combinado e a tabela
// de tarifas usada. Sem o snapshot, um preço de semanas atrás fica indefensável quando a
// tabela muda.
func TestSplitTransferRecordedWithFeeSnapshot(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Split", "owner@split.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Split", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 10000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar")
	}
	body := buyViaSession(t, ts, buyer(t, ts, pool, "compra@split.com"),
		map[string]any{"event_id": eventID, "quantity": 1}, "pix")
	ref := body["asaas_ref"].(string)

	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		var face, conv, margem int64
		var status, snapshot string
		if err := tx.QueryRow(ctx, `
			SELECT face_cents, convenience_cents, platform_margin_cents, split_status, fee_snapshot::text
			  FROM split_transfers`).Scan(&face, &conv, &margem, &status, &snapshot); err != nil {
			t.Fatalf("ler repasse: %v", err)
		}
		if face != 10000 {
			t.Fatalf("o repasse deveria ser o face limpo (10000), veio %d", face)
		}
		if margem != 1000 {
			t.Fatalf("margem deveria ser 10%% do face, veio %d", margem)
		}
		if conv <= margem {
			t.Fatalf("a conveniência precisa cobrir margem + tarifa: conv=%d margem=%d", conv, margem)
		}
		if status != "PENDING" {
			t.Fatalf("o repasse nasce pendente até o gateway liquidar, veio %s", status)
		}
		if len(snapshot) < 10 {
			t.Fatalf("sem snapshot da tabela de tarifas: %q", snapshot)
		}
	})

	// Liquidação do split: o repasse fecha e guarda qual split foi.
	splitWebhook(t, ts, ref, "split_123", "PAYMENT_SPLIT_DONE", "")
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		var status, splitID string
		if err := tx.QueryRow(ctx, `SELECT split_status, COALESCE(asaas_split_id,'') FROM split_transfers`).
			Scan(&status, &splitID); err != nil {
			t.Fatalf("ler repasse: %v", err)
		}
		if status != "DONE" || splitID != "split_123" {
			t.Fatalf("esperava DONE/split_123, veio %s/%s", status, splitID)
		}
	})
}

// TestSplitDivergenceBlock: o bloqueio por divergência é cenário esperado — a cobrança é
// criada semanas antes de ser paga, e uma mudança na tabela de tarifas nesse intervalo faz
// um valor que passou na criação divergir na liquidação. Precisa virar trabalho visível.
func TestSplitDivergenceBlock(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Diverg", "owner@diverg.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Diverg", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 10000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar")
	}
	body := buyViaSession(t, ts, buyer(t, ts, pool, "diverg@x.com"),
		map[string]any{"event_id": eventID, "quantity": 1}, "pix")
	ref := body["asaas_ref"].(string)

	splitWebhook(t, ts, ref, "split_d", "PAYMENT_SPLIT_DIVERGENCE_BLOCK", "")
	assertSplitStatus(t, ctx, pool, pid, "BLOCKED")

	// Prazo expirado: o split é cancelado e o repasse vira resolução manual.
	splitWebhook(t, ts, ref, "split_d", "PAYMENT_SPLIT_DIVERGENCE_BLOCK_FINISHED", "")
	assertSplitStatus(t, ctx, pool, pid, "CANCELLED")
}

// TestSplitRefusedKeepsReason: recusa guarda o motivo — sem ele não há como saber se foi
// dado do recebedor, antecipação de recebíveis ou outra coisa.
func TestSplitRefusedKeepsReason(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Recusa", "owner@recusa.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Recusa", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 10000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar")
	}
	body := buyViaSession(t, ts, buyer(t, ts, pool, "recusa@x.com"),
		map[string]any{"event_id": eventID, "quantity": 1}, "pix")
	splitWebhook(t, ts, body["asaas_ref"].(string), "split_r", "PAYMENT_SPLIT_REFUSED",
		"RECEIVABLE_UNIT_AFFECTED_BY_EXTERNAL_CONTRACTUAL_EFFECT")

	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		var status, motivo string
		if err := tx.QueryRow(ctx, `SELECT split_status, COALESCE(refusal_reason,'') FROM split_transfers`).
			Scan(&status, &motivo); err != nil {
			t.Fatalf("ler repasse: %v", err)
		}
		if status != "REFUSED" || motivo == "" {
			t.Fatalf("recusa deveria guardar o motivo: %s / %q", status, motivo)
		}
	})
}

// TestWebhookIdempotentByEventID: a idempotência é pelo id do EVENTO. O gateway reenvia
// quando não recebe 200, e reprocessar liquidação ou estorno duplicaria efeito com dinheiro.
func TestWebhookIdempotentByEventID(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Idem", "owner@idem.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Idem", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 10000, 100, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar")
	}
	body := buyViaSession(t, ts, buyer(t, ts, pool, "idem@x.com"),
		map[string]any{"event_id": eventID, "quantity": 2}, "pix")
	ref := body["asaas_ref"].(string)

	// Id único por execução (o banco de teste é compartilhado), mas o MESMO nas três
	// entregas — é isso que o gateway faz ao reenviar.
	evento := map[string]any{
		"id": "evt_" + uuid.NewString(), "asaas_ref": ref, "confirmed": true, "type": "PAYMENT_CONFIRMED",
	}
	for i := 0; i < 3; i++ {
		if code, _ := do(t, ts, "POST", "/api/v1/webhooks/asaas", nil, evento); code != http.StatusOK {
			t.Fatalf("webhook %d", i)
		}
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets`); n != 2 {
		t.Fatalf("o mesmo evento reenviado deveria emitir 2 ingressos, veio %d", n)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// splitWebhook simula um evento de split vindo do gateway.
func splitWebhook(t *testing.T, ts *httptest.Server, ref, splitID, tipo, motivo string) {
	t.Helper()
	if code, body := do(t, ts, "POST", "/api/v1/webhooks/asaas", nil, map[string]any{
		"id": "evt_" + uuid.NewString(), "type": tipo, "asaas_ref": ref,
		"split_id": splitID, "refusal_reason": motivo,
	}); code != http.StatusOK {
		t.Fatalf("webhook de split (%s): %d %v", tipo, code, body)
	}
}

// assertSplitStatus confere o estado do repasse no schema do produtor.
func assertSplitStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid uuid.UUID, want string) {
	t.Helper()
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		var status string
		if err := tx.QueryRow(ctx, `SELECT split_status FROM split_transfers`).Scan(&status); err != nil {
			t.Fatalf("ler repasse: %v", err)
		}
		if status != want {
			t.Fatalf("esperava split %s, veio %s", want, status)
		}
	})
}
