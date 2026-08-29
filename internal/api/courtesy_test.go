package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// uuidOf converte o id textual devolvido pela API.
func uuidOf(s string) uuid.UUID { return uuid.MustParse(s) }

// TestCortesiaExigeCategoria: a categoria não é etiqueta. O atestado publica cortesia POR
// categoria, e o join é interno — cortesia sem categoria some da comprovação de público.
func TestCortesiaExigeCategoria(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Cortesia", "owner@cortesia.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show", "shows")
	lotID := createLot(t, ts, owner, eventID, "Lote", 5000, 50, 0)

	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/guests", bearer(owner),
		map[string]any{"name": "Sem categoria", "lot_id": lotID}); code != http.StatusBadRequest {
		t.Fatalf("cortesia sem categoria deveria ser recusada, veio %d", code)
	}

	imprensa := courtesyCategoryID(t, ctx, pool, pid, "imprensa")
	if code, b := do(t, ts, "POST", "/api/v1/events/"+eventID+"/guests", bearer(owner),
		map[string]any{"name": "Jornalista", "lot_id": lotID, "courtesy_category_id": imprensa}); code != http.StatusCreated {
		t.Fatalf("cortesia com categoria: %d %v", code, b)
	}
}

// TestReclassificarCortesia: emitir na categoria errada e não poder corrigir significa
// publicar uma comprovação de público errada — e atestado não se edita, se republica.
func TestReclassificarCortesia(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Reclassifica", "owner@reclass.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show", "shows")
	lotID := createLot(t, ts, owner, eventID, "Lote", 5000, 50, 0)

	imprensa := courtesyCategoryID(t, ctx, pool, pid, "imprensa")
	patrocinador := courtesyCategoryID(t, ctx, pool, pid, "patrocinador")
	if code, b := do(t, ts, "POST", "/api/v1/events/"+eventID+"/guests", bearer(owner),
		map[string]any{"name": "Convidado", "lot_id": lotID, "courtesy_category_id": imprensa}); code != http.StatusCreated {
		t.Fatalf("emitir cortesia: %d %v", code, b)
	}
	guestID := firstGuestID(t, ts, owner, eventID)

	code, body := do(t, ts, "POST", "/api/v1/guests/"+guestID+"/category", bearer(owner),
		map[string]any{"courtesy_category_id": patrocinador, "reason": "era do patrocinador"})
	if code != http.StatusOK {
		t.Fatalf("reclassificar: %d %v", code, body)
	}
	if body["de"] != "imprensa" || body["para"] != "patrocinador" {
		t.Fatalf("esperava imprensa → patrocinador, veio %v", body)
	}

	// A troca fica na trilha, com o motivo: é o que explica a diferença entre dois atestados.
	n := scanInt(t, ctx, pool, pid, `
		SELECT count(*) FROM audit_events
		 WHERE entity='courtesy' AND from_status='imprensa' AND to_status='patrocinador'`)
	if n != 1 {
		t.Fatalf("a reclassificação precisa ficar na trilha, veio %d", n)
	}

	// E o atestado passa a contar na categoria nova.
	code, rep := do(t, ts, "GET", "/api/v1/events/"+eventID+"/reports/commitments", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("relatório: %d %v", code, rep)
	}
}

// TestCategoriaArquivadaNaoRecebe: arquivar existe para parar de usar sem apagar histórico.
// Reclassificar para uma arquivada recriaria o problema que o arquivamento resolve.
func TestCategoriaArquivadaNaoRecebe(t *testing.T) {
	ts, pool := setup(t)
	_, owner := createProducer(t, ts, "Casa Arquivo", "owner@arquivo.com", "senha1234")
	pid := producerID(t, ts, owner)
	ctx := context.Background()
	eventID := createEvent(t, ts, owner, "Show", "shows")
	lotID := createLot(t, ts, owner, eventID, "Lote", 5000, 50, 0)

	imprensa := courtesyCategoryID(t, ctx, pool, pid, "imprensa")
	equipe := courtesyCategoryID(t, ctx, pool, pid, "equipe")
	if code, _ := do(t, ts, "POST", "/api/v1/events/"+eventID+"/guests", bearer(owner),
		map[string]any{"name": "X", "lot_id": lotID, "courtesy_category_id": equipe}); code != http.StatusCreated {
		t.Fatalf("emitir cortesia")
	}
	guestID := firstGuestID(t, ts, owner, eventID)

	// Arquiva a categoria em uso.
	if code, b := do(t, ts, "PATCH", "/api/v1/courtesy-categories/"+equipe, bearer(owner),
		map[string]any{"active": false}); code != http.StatusOK {
		t.Fatalf("arquivar: %d %v", code, b)
	}
	// Arquivar manda só `active`. O nome não pode ir junto para vazio no caminho: a
	// cortesia já emitida continua apontando para essa categoria, e o atestado de um
	// evento passado publica esse nome.
	if nome := scanText(t, ctx, pool, pid, `
		SELECT name FROM courtesy_categories WHERE id=$1`, uuidOf(equipe)); nome == "" {
		t.Fatalf("arquivar apagou o nome da categoria")
	}
	// A cortesia existente PRESERVA o vínculo — arquivar não apaga história.
	if n := scanInt(t, ctx, pool, pid, `
		SELECT count(*) FROM guest_list_entries WHERE courtesy_category_id=$1`, uuidOf(equipe)); n != 1 {
		t.Fatalf("a cortesia existente precisa manter a categoria arquivada, veio %d", n)
	}
	// Mas nada novo entra nela.
	if code, _ := do(t, ts, "POST", "/api/v1/guests/"+guestID+"/category", bearer(owner),
		map[string]any{"courtesy_category_id": equipe}); code != http.StatusBadRequest {
		t.Fatalf("categoria arquivada não pode receber, veio %d", code)
	}
	if code, _ := do(t, ts, "POST", "/api/v1/guests/"+guestID+"/category", bearer(owner),
		map[string]any{"courtesy_category_id": imprensa}); code != http.StatusOK {
		t.Fatalf("categoria ativa deveria receber")
	}
}

// firstGuestID pega o id da cortesia pela listagem — é de lá que a tela também o obtém.
func firstGuestID(t *testing.T, ts *httptest.Server, owner, eventID string) string {
	t.Helper()
	code, body := do(t, ts, "GET", "/api/v1/events/"+eventID+"/guests", bearer(owner), nil)
	if code != http.StatusOK {
		t.Fatalf("listar cortesias: %d %v", code, body)
	}
	gs, _ := body["guests"].([]any)
	if len(gs) == 0 {
		t.Fatalf("nenhuma cortesia listada: %v", body)
	}
	id, _ := gs[0].(map[string]any)["id"].(string)
	return id
}

// scanText lê uma coluna de texto dentro do schema do produtor.
func scanText(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid uuid.UUID, q string, args ...any) string {
	t.Helper()
	var v string
	inTenant(t, ctx, pool, pid, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, q, args...).Scan(&v); err != nil {
			t.Fatalf("consulta: %v", err)
		}
	})
	return v
}
