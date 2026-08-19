package api_test

import (
	"net/http"
	"testing"
)

// TestAdminLoginAndScope: o token de admin tem escopo próprio — não vale em rota de
// produtor, e o de produtor não vale em rota de admin.
func TestAdminLoginAndScope(t *testing.T) {
	ts, pool := setup(t)
	admin := seedAdmin(t, ts, pool, "super@admin.com", "super_admin")

	// /admin/me prova a sessão e o papel.
	if code, body := do(t, ts, "GET", "/api/v1/admin/me", admin, nil); code != http.StatusOK {
		t.Fatalf("admin me: %d %v", code, body)
	}

	// Token de admin em rota de produtor → 401 (escopo separado).
	if code, _ := do(t, ts, "GET", "/api/v1/me", admin, nil); code != http.StatusUnauthorized {
		t.Fatalf("admin em rota de produtor: esperava 401, veio %d", code)
	}

	// Token de produtor em rota de admin → 401.
	_, owner := createProducer(t, ts, "Casa Escopo", "owner@escopo.com", "senha1234")
	if code, _ := do(t, ts, "GET", "/api/v1/admin/me", bearer(owner), nil); code != http.StatusUnauthorized {
		t.Fatalf("produtor em rota de admin: esperava 401, veio %d", code)
	}

	// Login com senha errada → 401.
	if code, _ := do(t, ts, "POST", "/api/v1/admin/login", nil,
		map[string]any{"email": "super@admin.com", "password": "errada__"}); code != http.StatusUnauthorized {
		t.Fatalf("senha errada: esperava 401, veio %d", code)
	}
}

// TestAdminArtistsCrud cobre o catálogo global de artistas (admin).
func TestAdminArtistsCrud(t *testing.T) {
	ts, pool := setup(t)
	admin := seedAdmin(t, ts, pool, "art@admin.com", "admin")

	code, body := do(t, ts, "POST", "/api/v1/admin/artists", admin,
		map[string]any{"name": "Dj Fulano", "category": "shows"})
	if code != http.StatusCreated {
		t.Fatalf("criar artista: %d %v", code, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("artista sem id: %v", body)
	}

	if code, _ = do(t, ts, "GET", "/api/v1/admin/artists", admin, nil); code != http.StatusOK {
		t.Fatalf("listar artistas: %d", code)
	}

	// Suspender (moderação reativa) e conferir o status.
	if code, _ := do(t, ts, "POST", "/api/v1/admin/artists/"+id+"/status", admin,
		map[string]any{"status": "suspended"}); code != http.StatusOK {
		t.Fatalf("suspender artista: %d", code)
	}
	_, all := do(t, ts, "GET", "/api/v1/admin/artists?all=true", admin, nil)
	artists := all["artists"].([]any)
	if len(artists) != 1 || artists[0].(map[string]any)["status"] != "suspended" {
		t.Fatalf("esperava artista suspenso: %v", all)
	}
}

// TestModerationFlow: denúncia pública → fila do admin → resolução.
func TestModerationFlow(t *testing.T) {
	ts, pool := setup(t)
	admin := seedAdmin(t, ts, pool, "mod@admin.com", "admin")

	// Denúncia pública (não exige auth).
	code, body := do(t, ts, "POST", "/api/v1/public/moderation/flags", nil,
		map[string]any{"target_type": "event", "target_id": "00000000-0000-0000-0000-000000000001", "reason": "conteúdo impróprio"})
	if code != http.StatusCreated {
		t.Fatalf("criar denúncia: %d %v", code, body)
	}
	flagID, _ := body["id"].(string)

	// Fila pendente.
	code, queue := do(t, ts, "GET", "/api/v1/admin/moderation/queue", admin, nil)
	if code != http.StatusOK || len(queue["flags"].([]any)) != 1 {
		t.Fatalf("fila de moderação: %d %v", code, queue)
	}

	// Resolve.
	if code, _ := do(t, ts, "PATCH", "/api/v1/admin/moderation/"+flagID, admin,
		map[string]any{"status": "resolved"}); code != http.StatusOK {
		t.Fatalf("resolver denúncia: %d", code)
	}

	// Ação auditada.
	code, logBody := do(t, ts, "GET", "/api/v1/admin/audit-log", admin, nil)
	if code != http.StatusOK || len(logBody["entries"].([]any)) == 0 {
		t.Fatalf("audit log: %d %v", code, logBody)
	}
}

// TestAdminRoleGating: só super_admin gere operadores.
func TestAdminRoleGating(t *testing.T) {
	ts, pool := setup(t)
	super := seedAdmin(t, ts, pool, "super@gate.com", "super_admin")
	plain := seedAdmin(t, ts, pool, "plain@gate.com", "admin")

	// admin comum não lista/cria admins.
	if code, _ := do(t, ts, "GET", "/api/v1/admin/admins", plain, nil); code != http.StatusForbidden {
		t.Fatalf("admin comum em /admins: esperava 403, veio %d", code)
	}
	// super_admin cria um operador.
	if code, _ := do(t, ts, "POST", "/api/v1/admin/admins", super,
		map[string]any{"email": "novo@gate.com", "password": "senha1234", "role": "admin"}); code != http.StatusCreated {
		t.Fatalf("super criar admin: %d", code)
	}
}
