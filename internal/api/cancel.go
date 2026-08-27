package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-timbre/internal/attest"
	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/catalog"
	"github.com/jadersonmarc/sapienza-timbre/internal/checkout"
	"github.com/jadersonmarc/sapienza-timbre/internal/notify"
)

// cancelEvent cancela o evento E devolve o dinheiro de todo mundo.
//
// Cancelar era só mudar o status: o evento sumia do diretório e cada comprador ficava com
// ingresso válido e dinheiro pago, descobrindo sozinho. O produtor achava que tinha
// resolvido.
//
// A devolução não acontece aqui. Um evento de mil ingressos seriam mil chamadas ao gateway
// com o produtor olhando para uma tela travada, e um timeout no meio deixaria metade
// devolvida sem registro de onde parou. Enfileira, avisa todo mundo, e o worker devolve.
func (s *Server) cancelEvent(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var enqueued int
	var buyers []string
	var title string
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		// Quem avisar precisa ser lido ANTES do estorno mexer no status dos pedidos.
		var e error
		if buyers, title, e = checkout.CancelledOrderBuyers(r.Context(), tx, eventID); e != nil {
			return e
		}
		if e := catalog.TransitionEvent(r.Context(), tx, eventID, "cancelled"); e != nil {
			return e
		}
		if enqueued, e = checkout.EnqueueEventRefunds(r.Context(), tx, eventID); e != nil {
			return e
		}
		// Evento já fechado: o registro canônico vai mudar. Republicar UMA vez ao fim do
		// lote, não a cada devolução — senão o atestado vira uma versão por pedido.
		return checkout.MarkAttestationStale(r.Context(), tx, eventID)
	}); err != nil {
		if errors.Is(err, catalog.ErrInvalidTransition) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// O aviso sai agora, não no fim da devolução: quem tinha ingresso para amanhã precisa
	// saber hoje, mesmo que o dinheiro leve dias para voltar.
	s.notifyCancelled(r.Context(), eventID, title, buyers)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "status": "cancelled", "refunds_queued": enqueued,
	})
}

func (s *Server) notifyCancelled(ctx context.Context, eventID uuid.UUID, title string, buyers []string) {
	if s.seams.Notify == nil {
		return
	}
	for _, to := range buyers {
		if err := s.seams.Notify.Send(ctx, notify.Message{
			Kind: notify.KindEventCancelled, To: to, EventName: title, EventID: &eventID,
		}); err != nil {
			slog.Warn("avisar cancelamento", "evento", eventID, "err", err)
		}
	}
}

// cancelProgress é o andamento do lote, para o produtor não ficar no escuro enquanto
// centenas de devoluções correm.
func (s *Server) cancelProgress(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var p checkout.CancelProgress
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		p, e = checkout.EventCancelProgress(r.Context(), tx, eventID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// adminFailedRefunds é a fila de resolução manual: devolução que esgotou as tentativas.
// Dinheiro que não voltou precisa de alguém, não de silêncio.
func (s *Server) adminFailedRefunds(w http.ResponseWriter, r *http.Request, _ *auth.AdminClaims) {
	type row struct {
		JobID        uuid.UUID `json:"job_id"`
		ProducerID   uuid.UUID `json:"producer_id"`
		ProducerName string    `json:"producer_name"`
		EventTitle   string    `json:"event_title"`
		OrderID      uuid.UUID `json:"order_id"`
		BuyerEmail   string    `json:"buyer_email"`
		TotalCents   int64     `json:"total_cents"`
		Attempts     int       `json:"attempts"`
		LastError    string    `json:"last_error"`
		UpdatedAt    time.Time `json:"updated_at"`
	}
	producers, err := listProducerRefs(r.Context(), s)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []row{}
	for _, p := range producers {
		if err := s.withTenant(r.Context(), p.id, func(tx pgx.Tx) error {
			rows, e := tx.Query(r.Context(), `
				SELECT j.id, e.title, j.order_id, COALESCE(o.buyer_email,''), o.total_cents,
				       j.attempts, COALESCE(j.last_error,''), j.updated_at
				  FROM refund_jobs j
				  JOIN events e ON e.id = j.event_id
				  JOIN orders o ON o.id = j.order_id
				 WHERE j.status='failed'
				 ORDER BY j.updated_at DESC`)
			if e != nil {
				return e
			}
			defer rows.Close()
			for rows.Next() {
				r := row{ProducerID: p.id, ProducerName: p.name}
				if e := rows.Scan(&r.JobID, &r.EventTitle, &r.OrderID, &r.BuyerEmail,
					&r.TotalCents, &r.Attempts, &r.LastError, &r.UpdatedAt); e != nil {
					return e
				}
				out = append(out, r)
			}
			return rows.Err()
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"failed": out})
}

// adminRetryRefundJob devolve uma falha à fila depois de alguém resolver a causa (saldo na
// conta do produtor, cadastro do comprador).
func (s *Server) adminRetryRefundJob(w http.ResponseWriter, r *http.Request, _ *auth.AdminClaims) {
	producerID, err := uuid.Parse(r.PathValue("producerId"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "produtor inválido")
		return
	}
	jobID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "devolução inválida")
		return
	}
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		return checkout.ReopenCancelJob(r.Context(), tx, jobID)
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ── worker ───────────────────────────────────────────────────────────────────

// CancelWorker devolve o dinheiro dos eventos cancelados, em segundo plano.
//
// Mora no pacote da API, e não junto das outras filas, por um motivo concreto: a devolução
// é feita em DUAS FASES (grava a intenção, commita, chama o gateway fora de transação,
// aplica os efeitos), e essa orquestração já vive aqui. Duplicá-la num pacote separado seria
// manter dois caminhos para o mesmo dinheiro.
//
// A forma segue o AnchorWorker — tentativas, backoff exponencial com teto e motivo
// persistido —, com uma diferença deliberada: lá a chamada externa acontece dentro da
// transação, e aqui não pode. Um estorno órfão é dinheiro perdido.
type CancelWorker struct {
	pool        *pgxpool.Pool
	srv         *Server
	interval    time.Duration
	batch       int
	MaxAttempts int
	Backoff     func(attempts int) time.Duration
}

// NewCancelWorker constrói o worker do cancelamento.
func NewCancelWorker(pool *pgxpool.Pool, srv *Server) *CancelWorker {
	return &CancelWorker{
		pool: pool, srv: srv, interval: 15 * time.Second, batch: 10,
		MaxAttempts: checkout.DefaultCancelMaxAttempts, Backoff: checkout.CancelBackoff,
	}
}

// Run processa devoluções pendentes até o contexto encerrar.
func (w *CancelWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.processAll(ctx); err != nil {
			slog.Warn("cancel worker", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ProcessTenant processa as devoluções pendentes de um produtor (exposto para testes).
func (w *CancelWorker) ProcessTenant(ctx context.Context, tenantID uuid.UUID) error {
	return w.processTenant(ctx, tenantID)
}

func (w *CancelWorker) processAll(ctx context.Context) error {
	schemas, err := tenancy.ListTenantSchemas(ctx, w.pool)
	if err != nil {
		return err
	}
	for _, tid := range schemas {
		if err := w.processTenant(ctx, tid); err != nil {
			slog.Warn("cancel worker tenant", "tenant", tid, "err", err)
		}
	}
	return nil
}

func (w *CancelWorker) processTenant(ctx context.Context, producerID uuid.UUID) error {
	// Transação curta só para reservar o lote. O gateway fica FORA dela.
	var jobs []checkout.CancelJob
	if err := w.srv.withTenant(ctx, producerID, func(tx pgx.Tx) error {
		var e error
		jobs, e = checkout.ClaimCancelJobs(ctx, tx, w.batch)
		return e
	}); err != nil {
		return err
	}
	for _, j := range jobs {
		w.processOne(ctx, producerID, j)
	}
	return w.republishStale(ctx, producerID)
}

func (w *CancelWorker) processOne(ctx context.Context, producerID uuid.UUID, j checkout.CancelJob) {
	// Cancelamento é decisão da plataforma em termos de autorização, ainda que disparada
	// pelo produtor: as guardas do produtor não se aplicam. Ingresso que já entrou também
	// é devolvido — o evento não aconteceu.
	var request checkout.RefundRequest
	if err := w.srv.withTenant(ctx, producerID, func(tx pgx.Tx) error {
		var e error
		request, e = checkout.CreateRequest(ctx, tx, checkout.NewRefundRequest{
			OrderID: j.OrderID, RequestedBy: checkout.RequesterAdmin,
			Reason: "evento cancelado", Actor: "cancelamento",
		})
		return e
	}); err != nil {
		// Pedido vivo do comprador na mesma compra: o caminho dele resolve, e insistir aqui
		// criaria duas devoluções para o mesmo dinheiro.
		if errors.Is(err, checkout.ErrRequestOpen) {
			w.finish(ctx, producerID, j, nil)
			return
		}
		if errors.Is(err, checkout.ErrRefundNothing) || errors.Is(err, checkout.ErrRefundNotPaid) {
			w.finish(ctx, producerID, j, nil) // já devolvido por outro caminho
			return
		}
		w.retry(ctx, producerID, j, err)
		return
	}

	if _, err := w.srv.runRefund(ctx, producerID, checkout.RefundInput{
		OrderID:        j.OrderID,
		InitiatedBy:    checkout.RefundByAdmin,
		Reason:         "evento cancelado",
		AllowCheckedIn: true,
	}, &request.ID); err != nil {
		w.retry(ctx, producerID, j, err)
		return
	}
	w.finish(ctx, producerID, j, &request.ID)
}

func (w *CancelWorker) finish(ctx context.Context, producerID uuid.UUID, j checkout.CancelJob, requestID *uuid.UUID) {
	if err := w.srv.withTenant(ctx, producerID, func(tx pgx.Tx) error {
		return checkout.FinishCancelJob(ctx, tx, j.ID, requestID)
	}); err != nil {
		slog.Error("encerrar devolução do cancelamento", "job", j.ID, "err", err)
	}
}

func (w *CancelWorker) retry(ctx context.Context, producerID uuid.UUID, j checkout.CancelJob, cause error) {
	attempts := j.Attempts + 1
	if err := w.srv.withTenant(ctx, producerID, func(tx pgx.Tx) error {
		return checkout.RetryCancelJob(ctx, tx, j.ID, attempts, w.MaxAttempts,
			cause.Error(), w.Backoff(attempts))
	}); err != nil {
		slog.Error("reagendar devolução do cancelamento", "job", j.ID, "err", err)
	}
}

// republishStale republica o registro canônico dos eventos cujo lote terminou. Uma vez, no
// fim — republicar a cada devolução geraria uma versão por pedido, e o atestado viraria
// ruído em vez de prova.
func (w *CancelWorker) republishStale(ctx context.Context, producerID uuid.UUID) error {
	var events []uuid.UUID
	if err := w.srv.withTenant(ctx, producerID, func(tx pgx.Tx) error {
		var e error
		events, e = checkout.StaleAttestations(ctx, tx)
		return e
	}); err != nil {
		return err
	}
	for _, id := range events {
		if err := w.srv.withTenant(ctx, producerID, func(tx pgx.Tx) error {
			if _, e := attest.Close(ctx, tx, w.srv.attest, w.srv.anchorer, w.srv.anchorMode,
				w.srv.attestKeyID, producerID, id); e != nil {
				return e
			}
			return checkout.ClearAttestationStale(ctx, tx, id)
		}); err != nil {
			slog.Error("republicar atestado após cancelamento", "evento", id, "err", err)
		}
	}
	return nil
}
