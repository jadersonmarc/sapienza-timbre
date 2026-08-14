package api

import (
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

// secureHeaders aplica cabeçalhos de segurança à superfície inteira (§4.1). A API só
// devolve JSON, então a CSP é restritiva (nada carrega recurso). O site (Next.js) tem a
// sua própria CSP para HTML.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}

// rateLimiter é um limitador de janela fixa, por chave, em memória. Suficiente para o
// deploy de instância única do Timbre (a fonte de verdade nunca é memória — aqui é só
// contenção de abuso). PROVISÓRIO: limites por rota calibráveis.
type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string]*rlWindow
	limit  int
	window time.Duration
}

type rlWindow struct {
	count int
	reset time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{hits: make(map[string]*rlWindow), limit: limit, window: window}
}

// allow contabiliza um acesso à chave e diz se está dentro do limite da janela.
func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	wnd, ok := l.hits[key]
	if !ok || now.After(wnd.reset) {
		l.hits[key] = &rlWindow{count: 1, reset: now.Add(l.window)}
		// Limpeza oportunista de chaves vencidas para não vazar memória.
		if len(l.hits) > 4096 {
			for k, v := range l.hits {
				if now.After(v.reset) {
					delete(l.hits, k)
				}
			}
		}
		return true
	}
	if wnd.count >= l.limit {
		return false
	}
	wnd.count++
	return true
}

// rateLimited embrulha um handler com o limite por IP+rota. Chave inclui o nome para que
// rotas diferentes não dividam o mesmo balde.
func (s *Server) rateLimited(name string, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(name + "|" + clientIP(r)) {
			writeErr(w, http.StatusTooManyRequests, "muitas requisições; tente novamente em instantes")
			return
		}
		fn(w, r)
	}
}

// deleteMe é a exclusão de conta do comprador (LGPD, §4.1): apaga o vínculo pessoa↔carteira
// e os dados pessoais do subject, PRESERVANDO o que é obrigação fiscal do produtor (ordens/
// ingressos no schema do produtor ficam intactos; só o índice público perde o vínculo/PII).
func (s *Server) deleteMe(w http.ResponseWriter, r *http.Request, subjectID uuid.UUID) {
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var email *string
	if err := tx.QueryRow(ctx, `SELECT email FROM subjects WHERE id=$1`, subjectID).Scan(&email); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM wallets WHERE subject_id=$1`, subjectID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE ticket_directory SET subject_id=NULL, buyer_email=NULL WHERE subject_id=$1`, subjectID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if email != nil {
		if _, err := tx.Exec(ctx, `DELETE FROM buyer_otps WHERE lower(email)=lower($1)`, *email); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	// Anonimiza o subject (não apaga a linha: attendance/consents referenciam por id solto).
	if _, err := tx.Exec(ctx, `UPDATE subjects SET email=NULL, cpf=NULL, display_name=NULL WHERE id=$1`, subjectID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
