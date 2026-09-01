package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestCortesiaPorFormularioEntregaEIdentificaOProdutor (B.8.7): a cortesia com e-mail
// ENTREGA o ingresso, e o aviso diz quem emitiu — é dado pessoal de terceiro que entrou no
// sistema pela mão do produtor.
func TestCortesiaPorFormularioEntregaEIdentificaOProdutor(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa do Beco", "owner@beco.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Cortesia", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 20, 0)
	cat := categoriaCortesia(t, ts, owner, "imprensa")

	// Categoria é obrigatória: sem default silencioso.
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/guests", bearer(owner),
		map[string]any{"name": "Sem Categoria"}); code != http.StatusBadRequest {
		t.Fatalf("cortesia sem categoria deveria ser recusada, veio %d", code)
	}

	code, body := do(t, ts, "POST", "/api/v1/events/"+eventID+"/guests", bearer(owner), map[string]any{
		"name": "Ana Repórter", "email": "ana@jornal.com", "phone": "31999998888",
		"courtesy_category_id": cat,
	})
	if code != http.StatusCreated {
		t.Fatalf("emitir cortesia: %d %v", code, body)
	}

	evID := uuid.MustParse(eventID)
	if n := scanInt(t, ctx, pool, pid, `
		SELECT count(*) FROM guest_list_entries
		 WHERE event_id=$1 AND email='ana@jornal.com' AND phone='31999998888'`, evID); n != 1 {
		t.Fatalf("contato do destinatário não foi registrado, veio %d", n)
	}

	// O aviso saiu, com o tipo próprio de cortesia e o nome de quem emitiu.
	var kind, payload string
	if err := pool.QueryRow(ctx, `
		SELECT kind, payload::text FROM public.notifications
		 WHERE to_email='ana@jornal.com' ORDER BY created_at DESC LIMIT 1`).Scan(&kind, &payload); err != nil {
		t.Fatalf("aviso da cortesia não foi enfileirado: %v", err)
	}
	if kind != "courtesy_issued" {
		t.Fatalf("esperava aviso de cortesia, veio %q", kind)
	}
	if !contains(payload, "Casa do Beco") {
		t.Fatalf("o aviso precisa dizer QUEM emitiu: %s", payload)
	}
}

// TestCortesiaEmLoteNaoParaNoPrimeiroErro (B.8.7): numa lista de convidados, um nome vazio
// não pode derrubar os que deram certo — e o resultado diz linha a linha o que aconteceu.
func TestCortesiaEmLoteNaoParaNoPrimeiroErro(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Lote", "owner@lotecortesia.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Lista", "shows")
	_ = createLot(t, ts, owner, eventID, "Lote 1", 5000, 20, 0)
	cat := categoriaCortesia(t, ts, owner, "convidado")

	code, body := do(t, ts, "POST", "/api/v1/events/"+eventID+"/guests/batch", bearer(owner), map[string]any{
		"courtesy_category_id": cat,
		"guests": []map[string]any{
			{"name": "Um", "email": "um@x.com"},
			{"name": ""},
			{"name": "Três", "email": "tres@x.com"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("lote: %d %v", code, body)
	}
	if body["issued"] != float64(2) {
		t.Fatalf("esperava 2 emitidos, veio %v", body["issued"])
	}
	linhas := body["results"].([]any)
	if len(linhas) != 3 {
		t.Fatalf("o resultado precisa falar de cada linha: %v", linhas)
	}
	if linhas[1].(map[string]any)["error"] == "" {
		t.Fatalf("a linha sem nome precisa dizer por quê: %v", linhas[1])
	}
	if n := scanInt(t, ctx, pool, pid, `SELECT count(*) FROM guest_list_entries WHERE event_id=$1`,
		uuid.MustParse(eventID)); n != 2 {
		t.Fatalf("esperava 2 convidados registrados, veio %d", n)
	}
}

// TestLinkOcultoNaoAparecePublicamenteERespeitaOsLimites (B.8.8): a categoria com link
// exclusivo não aparece na página, respeita limite e validade, e para de funcionar ao ser
// revogada.
func TestLinkOcultoNaoAparecePublicamenteERespeitaOsLimites(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Oculta", "owner@oculta.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show Oculto", "shows")
	publico := createLot(t, ts, owner, eventID, "Pista", 5000, 10, 0)
	oculto := criarLote(t, ts, owner, eventID, map[string]any{
		"name": "Lista do parceiro", "price_cents": 3000, "quantity": 5, "sort_order": 1,
		"availability": "always",
	})
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/publish", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("publicar")
	}

	code, link := do(t, ts, "POST", "/api/v1/lots/"+oculto+"/links", bearer(owner), map[string]any{
		"label": "parceiro", "max_uses": 1,
	})
	if code != http.StatusCreated {
		t.Fatalf("criar link: %d %v", code, link)
	}
	token, _ := link["token"].(string)
	if len(token) < 32 {
		t.Fatalf("token curto demais para não ser chutado: %q", token)
	}

	// Não aparece na página pública — nem na lista, nem como vigente.
	_, pub := do(t, ts, "GET", "/api/v1/public/events/"+eventID, nil, nil)
	for _, raw := range pub["lots"].([]any) {
		if raw.(map[string]any)["id"] == oculto {
			t.Fatalf("categoria oculta não pode aparecer na página: %v", pub["lots"])
		}
	}
	if pub["current_lot_id"] != publico && pub["current"] != publico {
		// o campo do vigente muda de nome conforme a versão do payload; basta não ser o oculto
		if pub["current_lot_id"] == oculto || pub["current"] == oculto {
			t.Fatalf("a categoria oculta não pode ser a vigente: %v", pub)
		}
	}

	// Com o link, ela abre.
	if code, b := do(t, ts, "GET", "/api/v1/public/events/"+eventID+"/hidden-lot?k="+token, nil, nil); code != http.StatusOK {
		t.Fatalf("o link deveria abrir a categoria: %d %v", code, b)
	}
	// Token errado não abre — e a resposta não distingue "não existe" de "revogado".
	if code, _ := do(t, ts, "GET", "/api/v1/public/events/"+eventID+"/hidden-lot?k=naoexiste", nil, nil); code != http.StatusNotFound {
		t.Fatalf("token desconhecido deveria dar 404, veio %d", code)
	}

	// Comprar SEM o token é recusado, mesmo com o id do lote em mãos.
	if code, _ := do(t, ts, "POST", "/api/v1/public/checkout/sessions", nil, map[string]any{
		"event_id": eventID, "lot_id": oculto, "quantity": 1,
	}); code < 400 {
		t.Fatalf("categoria oculta não pode ser comprada sem o link, veio %d", code)
	}

	// Com o token, compra. E o uso é contado na CONFIRMAÇÃO.
	res := buyViaSession(t, ts, buyer(t, ts, pool, "buy@oculta.com"), map[string]any{
		"event_id": eventID, "lot_id": oculto, "link_token": token, "quantity": 1,
	}, "pix")
	if usos := scanInt(t, ctx, pool, pid, `SELECT used_count FROM lot_links`); usos != 0 {
		t.Fatalf("abrir o link não pode gastar a vaga de ninguém, veio %d", usos)
	}
	confirmWebhook(t, ts, res["asaas_ref"].(string))
	if usos := scanInt(t, ctx, pool, pid, `SELECT used_count FROM lot_links`); usos != 1 {
		t.Fatalf("esperava 1 uso contado na confirmação, veio %d", usos)
	}

	// Esgotado o limite, o link deixa de abrir.
	if code, _ := do(t, ts, "GET", "/api/v1/public/events/"+eventID+"/hidden-lot?k="+token, nil, nil); code != http.StatusNotFound {
		t.Fatalf("link com os usos esgotados deveria fechar, veio %d", code)
	}

	// Um segundo link, revogado: para de funcionar na hora.
	_, link2 := do(t, ts, "POST", "/api/v1/lots/"+oculto+"/links", bearer(owner), map[string]any{"label": "outro"})
	token2 := link2["token"].(string)
	if code, _ := do(t, ts, "GET", "/api/v1/public/events/"+eventID+"/hidden-lot?k="+token2, nil, nil); code != http.StatusOK {
		t.Fatalf("o link novo deveria abrir")
	}
	if code, b := do(t, ts, "POST", "/api/v1/lot-links/"+link2["id"].(string)+"/revoke", bearer(owner), nil); code != http.StatusOK {
		t.Fatalf("revogar: %d %v", code, b)
	}
	if code, _ := do(t, ts, "GET", "/api/v1/public/events/"+eventID+"/hidden-lot?k="+token2, nil, nil); code != http.StatusNotFound {
		t.Fatalf("link revogado precisa parar na hora, veio %d", code)
	}
}

// categoriaCortesia cria (ou reusa) uma categoria de cortesia e devolve o id.
func categoriaCortesia(t *testing.T, ts *httptest.Server, owner, slug string) string {
	t.Helper()
	code, body := do(t, ts, "GET", "/api/v1/courtesy-categories", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("categorias: %d %v", code, body)
	}
	for _, raw := range body["categories"].([]any) {
		c := raw.(map[string]any)
		if c["slug"] == slug {
			return c["id"].(string)
		}
	}
	t.Fatalf("categoria %q não encontrada: %v", slug, body)
	return ""
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && strings.Contains(haystack, needle)
}
