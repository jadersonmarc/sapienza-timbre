package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/checkout"
	"github.com/jadersonmarc/sapienza-timbre/internal/inventory"
	"github.com/jadersonmarc/sapienza-timbre/internal/market"
	"github.com/jadersonmarc/sapienza-timbre/internal/notify"
	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
	"github.com/jadersonmarc/sapienza-timbre/internal/pricing"
	"github.com/jadersonmarc/sapienza-timbre/internal/season"
	"github.com/jadersonmarc/sapienza-timbre/internal/subaccount"
)

// publicQuote devolve a decomposição de preço (valor de face + taxa de conveniência +
// total) sem criar ordem — a tela de checkout mostra o total antes de confirmar e recalcula
// quando o comprador troca o método (§4.3). Resolve o produtor pelo diretório.
func (s *Server) publicQuote(w http.ResponseWriter, r *http.Request) {
	var req checkout.Request
	if err := decode(w, r, &req); err != nil || req.EventID == uuid.Nil {
		writeErr(w, http.StatusBadRequest, "event_id e método obrigatórios")
		return
	}
	producerID, err := s.producerOfEvent(r.Context(), req.EventID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "evento não encontrado")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	feeTable, ok := s.currentFees(w, r)
	if !ok {
		return
	}
	req.Fees = feeTable
	var bd pricing.Breakdown
	err = s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		var e error
		bd, e = checkout.Quote(r.Context(), tx, producerID, req)
		return e
	})
	if err != nil {
		writeCheckoutErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bd)
}

// asaasWebhook recebe a confirmação de pagamento (endpoint global). Idempotente:
// resolve o produtor pelo payment_index e confirma. Valida o header quando há token.
func (s *Server) asaasWebhook(w http.ResponseWriter, r *http.Request) {
	if s.webhookToken != "" && !subtleCompare(r.Header.Get("asaas-access-token"), s.webhookToken) {
		writeErr(w, http.StatusUnauthorized, "token de webhook inválido")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	evt, err := s.seams.Payment.HandleWebhook(r.Context(), body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "webhook ilegível")
		return
	}
	// Idempotência pelo ID DO EVENTO: a mesma cobrança gera confirmação, liquidação de
	// split e estorno, e deduplicar por cobrança descartaria eventos legítimos.
	if evt.ID != "" {
		novo, err := s.firstTimeSeeingEvent(r.Context(), evt.ID, evt.Type)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !novo {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "duplicado": true})
			return
		}
	}

	// Cadastro de subconta é outro fluxo: não fala de cobrança, fala do produtor.
	if evt.Kind() == payment.EventKindAccount {
		s.handleAccountEvent(w, r, evt)
		return
	}
	// Liquidação/bloqueio de split move o repasse, não o pedido.
	if evt.Kind() == payment.EventKindSplit {
		s.handleSplitEvent(w, r, evt)
		return
	}

	// Interessam confirmação e estorno; os demais são reconhecidos com 200.
	if evt.AsaasRef == "" || (!evt.Confirmed && !evt.Refunded) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	producerID, kind, err := s.producerOfPayment(r.Context(), evt.AsaasRef)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true}) // ref desconhecida: ack
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	em := s.emitter(producerID)
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		switch {
		case evt.Refunded:
			if err := checkout.RefundPayment(r.Context(), tx, evt.AsaasRef); err != nil {
				return err
			}
			return notifyRefund(r.Context(), s.seams.Notify, tx, evt.AsaasRef)
		case kind == "resale":
			return market.ConfirmResale(r.Context(), tx, producerID, evt.AsaasRef)
		case kind == "season":
			return season.ConfirmPass(r.Context(), tx, em, producerID, evt.AsaasRef)
		default:
			_, e := checkout.ConfirmPayment(r.Context(), tx, em, producerID, evt.AsaasRef)
			return e
		}
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) producerOfEvent(ctx context.Context, eventID uuid.UUID) (uuid.UUID, error) {
	var pid uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT producer_id FROM public.event_directory WHERE event_id = $1`, eventID).Scan(&pid)
	return pid, err
}

// notifyRefund envia a confirmação de estorno ao comprador (assíncrono — nunca bloqueia o
// webhook). Estrutura pronta; texto simples.
func notifyRefund(ctx context.Context, n notify.Notifier, tx pgx.Tx, asaasRef string) error {
	if n == nil {
		return nil
	}
	var to string
	var eventName string
	var total int64
	err := tx.QueryRow(ctx, `
		SELECT o.buyer_email, e.title, o.total_cents
		  FROM orders o
		  JOIN events e ON e.id = o.event_id
		 WHERE o.id = (SELECT order_id FROM payments WHERE asaas_ref = $1)`, asaasRef).
		Scan(&to, &eventName, &total)
	if errors.Is(err, pgx.ErrNoRows) || to == "" {
		return nil
	}
	if err != nil {
		return err
	}
	return n.Send(ctx, notify.Message{
		Kind: notify.KindRefunded, To: to, EventName: eventName, OrderValueCents: total,
	})
}

func (s *Server) producerOfPayment(ctx context.Context, asaasRef string) (uuid.UUID, string, error) {
	var pid uuid.UUID
	var kind string
	err := s.pool.QueryRow(ctx, `SELECT producer_id, kind FROM public.payment_index WHERE asaas_ref = $1`, asaasRef).Scan(&pid, &kind)
	return pid, kind, err
}

// writeCheckoutErr mapeia os erros de domínio do checkout para status HTTP.
func writeCheckoutErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, checkout.ErrLotUnavailable):
		writeErr(w, http.StatusConflict, "lote indisponível")
	case errors.Is(err, checkout.ErrInsufficientStock), errors.Is(err, inventory.ErrSeatUnavailable):
		writeErr(w, http.StatusConflict, "ingressos/assentos indisponíveis")
	case errors.Is(err, checkout.ErrSeatsMismatch), errors.Is(err, checkout.ErrCoupon),
		errors.Is(err, checkout.ErrBadRequest), errors.Is(err, inventory.ErrSeatInvalid),
		errors.Is(err, inventory.ErrSeatBlocked), errors.Is(err, inventory.ErrAntiHole):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

// ── lista de convidados / cortesias ──────────────────────────────────────────

type createGuestReq struct {
	Name               string  `json:"name"`
	CPF                string  `json:"cpf"`
	LotID              *string `json:"lot_id"`
	SeatID             *string `json:"seat_id"`
	CourtesyCategoryID string  `json:"courtesy_category_id"`
}

// createGuest emite uma cortesia: registra o convidado e emite um ingresso (com
// assento específico opcional, ocupando-o para não ser vendido).
func (s *Server) createGuest(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body createGuestReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if body.Name == "" {
		writeErr(w, http.StatusBadRequest, "name obrigatório")
		return
	}
	categoryID, err := uuid.Parse(body.CourtesyCategoryID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "courtesy_category_id obrigatório")
		return
	}
	lotID, err := parseUUIDPtr(body.LotID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "lot_id inválido")
		return
	}
	seatID, err := parseUUIDPtr(body.SeatID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "seat_id inválido")
		return
	}
	var ticketID uuid.UUID
	em := s.emitter(claims.ProducerID)
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		ticketID, e = checkout.IssueCourtesy(r.Context(), tx, em, eventID, lotID, seatID, categoryID, body.Name, body.CPF)
		return e
	}); err != nil {
		if errors.Is(err, inventory.ErrSeatUnavailable) {
			writeErr(w, http.StatusConflict, "assento indisponível")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ticket_id": ticketID})
}

func (s *Server) listGuests(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	type guest struct {
		ID     uuid.UUID `json:"id"`
		Name   string    `json:"name"`
		CPF    *string   `json:"cpf,omitempty"`
		SeatID *string   `json:"seat_id,omitempty"`
		Status string    `json:"status"`
	}
	var out []guest
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		rows, e := tx.Query(r.Context(), `
			SELECT id, name, cpf, seat_id::text, status
			  FROM guest_list_entries WHERE event_id=$1 ORDER BY created_at`, eventID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var g guest
			if e := rows.Scan(&g.ID, &g.Name, &g.CPF, &g.SeatID, &g.Status); e != nil {
				return e
			}
			out = append(out, g)
		}
		return rows.Err()
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"guests": out})
}

func parseUUIDPtr(s *string) (*uuid.UUID, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	id, err := uuid.Parse(*s)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// currentFees devolve a tabela de tarifas para o cálculo de preço. Erro aqui é 503, não
// 500: é indisponibilidade externa, e o comprador pode tentar de novo em instantes.
func (s *Server) currentFees(w http.ResponseWriter, r *http.Request) (payment.Fees, bool) {
	if s.seams.Fees == nil {
		writeErr(w, http.StatusServiceUnavailable, "tabela de tarifas indisponível")
		return payment.Fees{}, false
	}
	f, err := s.seams.Fees.Current(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "não foi possível calcular o preço agora")
		return payment.Fees{}, false
	}
	return f, true
}

// firstTimeSeeingEvent registra o evento e diz se é a primeira vez que ele chega. A chave é
// o id do EVENTO — o gateway reenvia quando não recebe 200, e reprocessar um estorno ou uma
// liquidação duplicaria efeito com dinheiro.
func (s *Server) firstTimeSeeingEvent(ctx context.Context, eventID, eventType string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO public.webhook_events (event_id, event_type) VALUES ($1,$2)
		ON CONFLICT (event_id) DO NOTHING`, eventID, eventType)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// handleSplitEvent trata a vida do repasse depois da venda: liquidado, bloqueado por
// divergência, cancelado ou recusado.
//
// O bloqueio por divergência acontece quando o split ficou maior que o líquido no
// recebimento — a cobrança é criada semanas antes de ser paga, e uma mudança na tabela de
// tarifas nesse intervalo faz um valor que passou na criação divergir na liquidação. É
// cenário esperado, não defeito: há prazo para ajustar, e por isso vira alerta e não log.
func (s *Server) handleSplitEvent(w http.ResponseWriter, r *http.Request, evt payment.WebhookEvent) {
	if evt.AsaasRef == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	producerID, _, err := s.producerOfPayment(r.Context(), evt.AsaasRef)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := evt.SplitStatus
	if status == "" {
		status = checkout.SplitPending
	}
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		return checkout.MarkSplitStatus(r.Context(), tx, evt.AsaasRef, evt.SplitID, status, evt.RefusalReason)
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	switch status {
	case checkout.SplitBlocked:
		// Prazo curto para ajustar; passado ele, o split é cancelado e o repasse vira
		// resolução manual. Precisa chegar em quem opera, não só no arquivo de log.
		slog.Error("split bloqueado por divergência — ajuste dentro do prazo do gateway",
			"producer_id", producerID, "cobranca", evt.AsaasRef, "split", evt.SplitID)
	case checkout.SplitCancelled:
		slog.Error("split cancelado pelo gateway — repasse pendente de resolução manual",
			"producer_id", producerID, "cobranca", evt.AsaasRef, "split", evt.SplitID)
	case checkout.SplitRefused:
		slog.Error("split recusado pelo gateway", "producer_id", producerID,
			"cobranca", evt.AsaasRef, "motivo", evt.RefusalReason)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAccountEvent move a máquina de estados da subconta do produtor e agenda a
// confirmação anual de dados comerciais quando o gateway avisa que está por vencer.
func (s *Server) handleAccountEvent(w http.ResponseWriter, r *http.Request, evt payment.WebhookEvent) {
	if s.seams.Subaccounts == nil || evt.WalletID == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	ctx := r.Context()
	if evt.CommercialInfoExpiresAt != nil {
		if err := s.seams.Subaccounts.SetCommercialInfoExpiration(ctx, evt.WalletID, evt.CommercialInfoExpiresAt); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if status := accountStatusFor(evt); status != "" {
		if err := s.seams.Subaccounts.SetStatus(ctx, evt.WalletID, status, evt.RefusalReason); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if evt.Type == "ACCOUNT_STATUS_COMMERCIAL_INFO_EXPIRING_SOON" {
		slog.Warn("subconta: confirmação anual de dados comerciais por vencer — sem ela a conta perde o uso da API",
			"wallet", evt.WalletID, "vence_em", evt.CommercialInfoExpiresAt)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountStatusFor traduz a situação vinda do gateway para a nossa máquina de estados.
func accountStatusFor(evt payment.WebhookEvent) string {
	switch evt.AccountStatus {
	case "APPROVED":
		return subaccount.StatusApproved
	case "REJECTED":
		return subaccount.StatusRejected
	case "PENDING", "AWAITING_ACTION_AUTHORIZATION":
		return subaccount.StatusAnalysis
	}
	return ""
}
