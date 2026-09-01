package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestCotaDeMeiaVigoraSemDeclaracao: a cota de 40% da Lei 12.933/2013 vale mesmo sem o
// produtor declarar nada — a obrigação é da lei, não da declaração.
func TestCotaDeMeiaVigoraSemDeclaracao(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducer(t, ts, "Casa Meia", "owner@meia.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show Meia", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 10, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar")
	}

	code, body := do(t, ts, "GET", "/api/v1/public/events/"+eventID, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("evento público: %d", code)
	}
	hp, _ := body["half_price"].(map[string]any)
	// 40% de 10 lugares = 4. A página PRECISA informar isso: art. 1º, §1º obriga a exibir
	// a disponibilidade de meia em todos os pontos de venda.
	if hp["quota"] != float64(4) {
		t.Fatalf("esperava cota legal de 4, veio %v", hp)
	}
	if hp["available"] != true {
		t.Fatalf("cota intacta deveria estar disponível: %v", hp)
	}
}

// TestCotaDeMeiaAbaixoDoPisoAceitaComAvisoETrilha (B.8.2): 40% é o DEFAULT, não trava.
//
// Isto REVERTE a decisão anterior de travar o piso. A Lei 12.933/2013 obriga o produtor;
// recusar a configuração dele não o faz cumprir a lei, só o impede de operar e nos põe no
// lugar de fiscal. O que o sistema faz é avisar e registrar quem escolheu.
func TestCotaDeMeiaAbaixoDoPisoAceitaComAvisoETrilha(t *testing.T) {
	ts, _ := setup(t)
	_, owner := createProducer(t, ts, "Casa Piso", "owner@piso.com", "senha1234")
	eventID := createEvent(t, ts, owner, "Show", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 10, 0)

	code, body := do(t, ts, "PUT", "/api/v1/events/"+eventID+"/half-price", bearer(owner), map[string]any{
		"mode": "quota", "target_type": "percent", "target_value": "10",
	})
	if code != http.StatusOK {
		t.Fatalf("cota de 10%% deveria ser aceita: %d %v", code, body)
	}
	hp, _ := body["half_price"].(map[string]any)
	if hp["below_legal"] != true {
		t.Fatalf("esperava a marca de abaixo do piso legal: %v", hp)
	}
	if hp["quota"] != float64(1) {
		t.Fatalf("a cota escolhida é que vale (10%% de 10), veio %v", hp["quota"])
	}
	if hp["legal_quota"] != float64(4) {
		t.Fatalf("o aviso precisa do número da lei ao lado: %v", hp)
	}
	if aviso, _ := body["warning"].(string); aviso == "" {
		t.Fatalf("escolher abaixo do piso sem aviso é o pior dos dois mundos: %v", body)
	}

	// E fica na trilha: valor, data e usuário.
	code, hist := do(t, ts, "GET", "/api/v1/events/"+eventID+"/half-price/history", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("trilha: %d", code)
	}
	linhas, _ := hist["events"].([]any)
	if len(linhas) != 1 {
		t.Fatalf("esperava 1 linha na trilha, veio %v", hist["events"])
	}
	linha := linhas[0].(map[string]any)
	if linha["actor"] == nil || linha["actor"] == "" {
		t.Fatalf("trilha sem quem escolheu: %v", linha)
	}
	det, _ := linha["details"].(map[string]any)
	if det["target_value"] != "10" || det["below_legal"] != true {
		t.Fatalf("trilha sem o valor escolhido: %v", det)
	}
}

// TestMeiaVinculadaSegueOEstoqueDoPai (B.8.3): no modo vinculado a meia não tem cota
// própria — sai enquanto houver ingresso no lote pai. E continua consumindo esse estoque:
// estoque separado que SOMA é o que faz a casa ser vendida duas vezes.
func TestMeiaVinculadaSegueOEstoqueDoPai(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Vinculada", "owner@vinculada.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 4, 0)
	if code, b := do(t, ts, "PUT", "/api/v1/events/"+eventID+"/half-price", bearer(owner),
		map[string]any{"mode": "linked"}); code != http.StatusOK {
		t.Fatalf("vincular: %d %v", code, b)
	}
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar")
	}

	// A cota legal seria 2 de 4. Vinculada, as quatro podem ser meia.
	res := buyViaSession(t, ts, buyer(t, ts, pool, "buy@vinculada.com"), map[string]any{
		"event_id": eventID, "quantity": 4, "half_price_qty": 4,
	}, "pix")
	confirmWebhook(t, ts, res["asaas_ref"].(string))

	evID := uuid.MustParse(eventID)
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE event_id=$1 AND half_price`, evID); n != 4 {
		t.Fatalf("vinculada deveria emitir 4 meias, veio %d", n)
	}
	// E o estoque do pai foi consumido pelas quatro: o lote está cheio.
	if n := scanInt(t, ctx, pool, pid, `SELECT sold_count FROM lots WHERE event_id=$1`, evID); n != 4 {
		t.Fatalf("a meia precisa consumir o estoque do pai, veio sold_count=%d", n)
	}
	_, pub := do(t, ts, "GET", "/api/v1/public/events/"+eventID, nil, nil)
	hp, _ := pub["half_price"].(map[string]any)
	if hp["mode"] != "linked" {
		t.Fatalf("a página pública precisa dizer o modo: %v", hp)
	}
}

// TestCotaDeMeiaBarraAVenda: o compromisso declarado deixa de ser enfeite do atestado e
// passa a valer na venda. Esgotada a cota, a meia sai e a inteira continua.
func TestCotaDeMeiaBarraAVenda(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Cota", "owner@cota.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Cota", "shows")
	// 5 lugares → cota legal de 2 meias (40%).
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 5, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar")
	}

	// Duas meias: cabe.
	res := buyViaSession(t, ts, buyer(t, ts, pool, "meia1@cota.com"), map[string]any{
		"event_id": eventID, "quantity": 2, "half_price_qty": 2,
	}, "pix")
	confirmWebhook(t, ts, res["asaas_ref"].(string))
	evID := uuid.MustParse(eventID)
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE event_id=$1 AND half_price`, evID); n != 2 {
		t.Fatalf("esperava 2 meias emitidas, veio %d", n)
	}

	// A terceira meia estoura a cota e é recusada NO SERVIDOR.
	code, body := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil, map[string]any{
		"event_id": eventID, "quantity": 1, "half_price_qty": 1,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("meia além da cota deveria ser recusada, veio %d %v", code, body)
	}
	if msg, _ := body["error"].(string); !containsAll(msg, "meia-entrada") {
		t.Fatalf("a recusa precisa dizer que a cota de meia acabou, veio %q", msg)
	}

	// A INTEIRA continua: acabou a meia, não o evento.
	if code, body := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil, map[string]any{
		"event_id": eventID, "quantity": 1,
	}); code != http.StatusCreated {
		t.Fatalf("a inteira deveria seguir à venda: %d %v", code, body)
	}

	// E a página anuncia que a meia acabou.
	_, pub := do(t, ts, "GET", "/api/v1/public/events/"+eventID, nil, nil)
	hp, _ := pub["half_price"].(map[string]any)
	if hp["available"] != false || hp["remaining"] != float64(0) {
		t.Fatalf("a página deveria anunciar a meia esgotada: %v", hp)
	}
}

// TestComboDeMeiaConsomeDoisDaCota: a cota conta INGRESSOS emitidos, não compras — um
// duplo de meia tira dois da cota, não um.
func TestComboDeMeiaConsomeDoisDaCota(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Combo Meia", "owner@combomeia.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	dois := 2
	eventID := comboEvent(t, ts, owner, 2, &dois, 10) // cota legal: 4 meias

	res := buyViaSession(t, ts, buyer(t, ts, pool, "buy@combomeia.com"), map[string]any{
		"event_id": eventID, "quantity": 2, "half_price_qty": 2,
	}, "pix")
	confirmWebhook(t, ts, res["asaas_ref"].(string))

	evID := uuid.MustParse(eventID)
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM tickets WHERE event_id=$1 AND half_price`, evID); n != 2 {
		t.Fatalf("esperava 2 meias emitidas pelo duplo, veio %d", n)
	}
	_, pub := do(t, ts, "GET", "/api/v1/public/events/"+eventID, nil, nil)
	hp, _ := pub["half_price"].(map[string]any)
	if hp["granted"] != float64(2) {
		t.Fatalf("o duplo de meia deveria consumir 2 da cota, veio %v", hp["granted"])
	}
	if hp["remaining"] != float64(2) {
		t.Fatalf("deveriam restar 2 da cota de 4, veio %v", hp["remaining"])
	}
}

// TestEstornoDevolveACota: ingresso de meia estornado volta para a cota — ele não foi
// usado por ninguém.
func TestEstornoDevolveACota(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Cota Estorno", "owner@cotaestorno.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 5, 0)
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar")
	}
	res := buyViaSession(t, ts, buyer(t, ts, pool, "buy@cotaestorno.com"), map[string]any{
		"event_id": eventID, "quantity": 2, "half_price_qty": 2,
	}, "pix")
	confirmWebhook(t, ts, res["asaas_ref"].(string))

	orderID := orderOf(t, ctx, pool, pid, uuid.MustParse(eventID))
	if code, b := do(t, ts, "POST", "/api/v1/orders/"+orderID+"/refund", bearer(owner),
		map[string]any{"reason": "devolveu"}); code != http.StatusOK {
		t.Fatalf("estorno: %d %v", code, b)
	}
	_, pub := do(t, ts, "GET", "/api/v1/public/events/"+eventID, nil, nil)
	hp, _ := pub["half_price"].(map[string]any)
	if hp["granted"] != float64(0) {
		t.Fatalf("meia estornada deveria voltar para a cota, veio %v", hp["granted"])
	}
}
