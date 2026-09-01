package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/catalog"
	"github.com/jadersonmarc/sapienza-timbre/internal/checkout"
	"github.com/jadersonmarc/sapienza-timbre/internal/inventory"
	"github.com/jadersonmarc/sapienza-timbre/internal/market"
	"github.com/jadersonmarc/sapienza-timbre/internal/notify"
	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
	"github.com/jadersonmarc/sapienza-timbre/internal/payout"
	"github.com/jadersonmarc/sapienza-timbre/internal/pricing"
	"github.com/jadersonmarc/sapienza-timbre/internal/season"
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
	// Idempotência pelo ID DO EVENTO: a mesma cobrança gera confirmação e estorno, e
	// deduplicar por cobrança descartaria eventos legítimos.
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
		var err error
		switch {
		case evt.Refunded:
			if err = checkout.RefundPayment(r.Context(), tx, evt.AsaasRef, evt.RefundKeys); err != nil {
				return err
			}
			err = notifyRefund(r.Context(), s.seams.Notify, tx, evt.AsaasRef)
		case kind == "resale":
			err = market.ConfirmResale(r.Context(), tx, producerID, evt.AsaasRef)
		case kind == "season":
			err = season.ConfirmPass(r.Context(), tx, em, producerID, evt.AsaasRef)
		default:
			_, err = checkout.ConfirmPayment(r.Context(), tx, em, producerID, evt.AsaasRef)
		}
		if err != nil {
			return err
		}
		// O dinheiro mudou: a obrigação de repasse do evento acompanha, na mesma transação.
		// Todo caminho de venda passa por aqui — compra comum, passe e revenda —, então é o
		// ponto onde nenhum deles escapa.
		return recomputePayoutOfPayment(r.Context(), tx, evt.AsaasRef)
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// recomputePayoutOfPayment recalcula a obrigação de repasse do evento a que a cobrança
// pertence. Cobrança sem pedido conhecido não é erro: o webhook chega para coisas que não
// nasceram aqui.
func recomputePayoutOfPayment(ctx context.Context, tx pgx.Tx, asaasRef string) error {
	var eventID uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT o.event_id FROM orders o
		  JOIN payments p ON p.order_id = o.id
		 WHERE p.asaas_ref = $1`, asaasRef).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = payout.Recompute(ctx, tx, eventID)
	return err
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
	return n.Send(ctx, tx, notify.Message{
		Kind: notify.KindRefunded, To: to, EventName: eventName, OrderValueCents: total,
		IdempotencyKey: "refunded:" + asaasRef,
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
	case errors.Is(err, catalog.ErrPurchaseRange), errors.Is(err, catalog.ErrBadPurchaseRange):
		writeErr(w, http.StatusBadRequest, err.Error())
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
	Name string `json:"name"`
	CPF  string `json:"cpf"`
	// Email é o que faz a emissão ENTREGAR. Telefone é opcional e fica só registrado: é o
	// contato que sobra quando o e-mail volta.
	Email              string  `json:"email"`
	Phone              string  `json:"phone"`
	LotID              *string `json:"lot_id"`
	SeatID             *string `json:"seat_id"`
	CourtesyCategoryID string  `json:"courtesy_category_id"`
}

// batchGuestReq é a emissão em lote por lista. Mesmo tratamento de cada um: categoria
// obrigatória, aviso identificando quem emitiu, uma linha por convidado.
type batchGuestReq struct {
	CourtesyCategoryID string           `json:"courtesy_category_id"`
	LotID              *string          `json:"lot_id"`
	Guests             []checkout.Guest `json:"guests"`
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
	producerName, err := s.producerName(r.Context(), claims.ProducerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var ticketID uuid.UUID
	em := s.emitter(claims.ProducerID)
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		ticketID, e = checkout.IssueCourtesy(r.Context(), tx, em, eventID, lotID, seatID, categoryID,
			checkout.Guest{Name: body.Name, CPF: body.CPF, Email: body.Email, Phone: body.Phone},
			producerName)
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
		// A categoria vem junto porque é por ela que o atestado conta a cortesia — e é o que
		// a tela precisa para reclassificar e para somar por categoria.
		CategoryID   *string `json:"courtesy_category_id,omitempty"`
		CategorySlug string  `json:"courtesy_category,omitempty"`
	}
	var out []guest
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		rows, e := tx.Query(r.Context(), `
			SELECT g.id, g.name, g.cpf, g.seat_id::text, g.status,
			       g.courtesy_category_id::text, COALESCE(cc.slug,'')
			  FROM guest_list_entries g
			  LEFT JOIN courtesy_categories cc ON cc.id = g.courtesy_category_id
			 WHERE g.event_id=$1 ORDER BY g.created_at`, eventID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var g guest
			if e := rows.Scan(&g.ID, &g.Name, &g.CPF, &g.SeatID, &g.Status,
				&g.CategoryID, &g.CategorySlug); e != nil {
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

// producerName lê o nome da casa. Vai no aviso da cortesia: quem recebe um e-mail com o
// próprio nome tem o direito de saber quem o enviou.
func (s *Server) producerName(ctx context.Context, producerID uuid.UUID) (string, error) {
	var name string
	err := s.pool.QueryRow(ctx, `SELECT name FROM public.producers WHERE id=$1`, producerID).Scan(&name)
	return name, err
}

// createGuestBatch emite cortesias em LOTE, por lista. Uma pessoa por linha, com o mesmo
// tratamento da emissão individual — categoria obrigatória e aviso identificando quem
// emitiu.
//
// Cada convidado é emitido em sua PRÓPRIA transação: numa lista de cem, um assento ocupado
// ou um nome vazio não pode derrubar os noventa e nove que deram certo. O resultado diz,
// linha a linha, o que aconteceu.
func (s *Server) createGuestBatch(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body batchGuestReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	categoryID, err := uuid.Parse(body.CourtesyCategoryID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "courtesy_category_id obrigatório")
		return
	}
	if len(body.Guests) == 0 {
		writeErr(w, http.StatusBadRequest, "informe pelo menos um convidado")
		return
	}
	if len(body.Guests) > maxCourtesyBatch {
		writeErr(w, http.StatusBadRequest, "no máximo "+strconv.Itoa(maxCourtesyBatch)+" convidados por lista")
		return
	}
	lotID, err := parseUUIDPtr(body.LotID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "lot_id inválido")
		return
	}
	producerName, err := s.producerName(r.Context(), claims.ProducerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	em := s.emitter(claims.ProducerID)
	type linha struct {
		Name     string     `json:"name"`
		Email    string     `json:"email,omitempty"`
		TicketID *uuid.UUID `json:"ticket_id,omitempty"`
		Error    string     `json:"error,omitempty"`
	}
	out := make([]linha, 0, len(body.Guests))
	emitidos := 0
	for _, g := range body.Guests {
		l := linha{Name: g.Name, Email: g.Email}
		if g.Name == "" {
			l.Error = "nome obrigatório"
			out = append(out, l)
			continue
		}
		var ticketID uuid.UUID
		if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
			var e error
			ticketID, e = checkout.IssueCourtesy(r.Context(), tx, em, eventID, lotID, nil, categoryID, g, producerName)
			return e
		}); err != nil {
			l.Error = err.Error()
			out = append(out, l)
			continue
		}
		l.TicketID = &ticketID
		emitidos++
		out = append(out, l)
	}
	writeJSON(w, http.StatusOK, map[string]any{"issued": emitidos, "results": out})
}

// maxCourtesyBatch limita a lista de uma vez. PROVISÓRIO: alto o bastante para uma lista de
// convidados real, baixo o bastante para a requisição não virar um lote de horas.
const maxCourtesyBatch = 500
