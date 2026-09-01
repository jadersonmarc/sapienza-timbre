package api

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/payout"
	"github.com/jadersonmarc/sapienza-timbre/internal/store"
)

// payoutDue é uma linha da fila de repasse: o que a plataforma deve de um EVENTO, para quem
// e até quando. Os dados bancários vêm INTEIROS aqui — quem opera precisa copiar a chave
// para fazer a transferência, e mascarar seria teatro num painel que já exige ser admin.
//
// A fila é por evento porque a obrigação é por evento: o dinheiro fica retido até a
// realização, e juntar tudo num saldo por produtor esconderia justamente o que decide se um
// valor já venceu.
type payoutDue struct {
	ProducerID   uuid.UUID `json:"producer_id"`
	ProducerName string    `json:"producer_name"`
	EventID      uuid.UUID `json:"event_id"`
	EventTitle   string    `json:"event_title"`

	NetDueCents       int64      `json:"net_due_cents"`
	GrossFaceCents    int64      `json:"gross_face_cents"`
	RefundedFaceCents int64      `json:"refunded_face_cents"`
	Status            string     `json:"status"`
	DueAt             *time.Time `json:"due_at"`
	// Overdue é o vencimento no passado com o repasse ainda em aberto. É o que transforma a
	// lista em fila de trabalho: sem isso, "pendente" e "atrasado" têm a mesma aparência.
	Overdue     bool   `json:"overdue"`
	HoldReason  string `json:"hold_reason,omitempty"`
	HoldMessage string `json:"hold_message,omitempty"`

	PixKey      string `json:"pix_key"`
	PixKeyType  string `json:"pix_key_type"`
	HolderName  string `json:"holder_name"`
	HolderTaxID string `json:"holder_tax_id"`
	// Blocked é ter dinheiro a transferir e nenhum destino cadastrado. Cobrar o cadastro
	// ANTES do vencimento evita a transferência atrasar por isso.
	Blocked bool `json:"blocked"`
}

// recoverableCredit é o contrário do repasse: estorno que chegou DEPOIS de o repasse do
// evento ter sido pago. Fica visível e NÃO é abatido de nada automaticamente — não há
// repasse futuro garantido, e um desconto silencioso no evento seguinte é o tipo de número
// que ninguém aceita.
type recoverableCredit struct {
	ProducerID   uuid.UUID `json:"producer_id"`
	ProducerName string    `json:"producer_name"`
	EventID      uuid.UUID `json:"event_id"`
	EventTitle   string    `json:"event_title"`
	OrderID      uuid.UUID `json:"order_id"`
	AmountCents  int64     `json:"amount_cents"`
	Reason       string    `json:"reason"`
	CreatedAt    time.Time `json:"created_at"`
}

// adminPayouts lista o que a plataforma deve, evento a evento, e os créditos a recuperar.
func (s *Server) adminPayouts(w http.ResponseWriter, r *http.Request, _ *auth.AdminClaims) {
	ctx := r.Context()
	producers, err := store.ListProducers(ctx, s.pool, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	list := make([]payoutDue, 0)
	credits := make([]recoverableCredit, 0)
	var totalDue, totalCredit int64
	now := time.Now()
	for _, p := range producers {
		acc, err := store.GetPayoutAccount(ctx, s.pool, p.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		var rows []payout.Payout
		var cs []recoverableCredit
		if err := s.withTenant(ctx, p.ID, func(tx pgx.Tx) error {
			var e error
			if rows, e = payout.List(ctx, tx); e != nil {
				return e
			}
			cs, e = openCredits(ctx, tx)
			return e
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, c := range cs {
			c.ProducerID, c.ProducerName = p.ID, p.Name
			totalCredit += c.AmountCents
			credits = append(credits, c)
		}
		for _, po := range rows {
			// Pago e cancelado saíram da fila: não são trabalho de ninguém. Evento sem
			// líquido também não.
			if po.Status == payout.StatusPaid || po.Status == payout.StatusCancelled || po.NetDueCents <= 0 {
				continue
			}
			d := payoutDue{
				ProducerID: p.ID, ProducerName: p.Name,
				EventID: po.EventID, EventTitle: po.EventTitle,
				NetDueCents: po.NetDueCents, GrossFaceCents: po.GrossFaceCents,
				RefundedFaceCents: po.RefundedFaceCents,
				Status:            po.Status, DueAt: po.DueAt,
				HoldReason: po.HoldReason, HoldMessage: po.HoldMessage,
				PixKey: acc.PixKey, PixKeyType: acc.PixKeyType,
				HolderName: acc.HolderName, HolderTaxID: acc.HolderTaxID,
				Blocked: acc.PixKey == "",
			}
			d.Overdue = po.Status == payout.StatusPending && po.DueAt != nil && po.DueAt.Before(now)
			if po.Status == payout.StatusPending {
				totalDue += po.NetDueCents
			}
			list = append(list, d)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_pending_cents": totalDue,
		"payouts":             list,
		"recoverable_credits": credits,
		"total_credit_cents":  totalCredit,
		// A transferência é feita por fora e marcada aqui. Dizer isso no corpo evita que a
		// tela sugira um botão que paga — não existe execução bancária no produto.
		"execution_is_manual": true,
	})
}

// openCredits lê os créditos a recuperar ainda em aberto do produtor.
func openCredits(ctx context.Context, tx pgx.Tx) ([]recoverableCredit, error) {
	rows, err := tx.Query(ctx, `
		SELECT c.event_id, e.title, c.order_id, c.amount_cents, COALESCE(c.reason,''), c.created_at
		  FROM recoverable_credits c JOIN events e ON e.id = c.event_id
		 WHERE c.settled_at IS NULL
		 ORDER BY c.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []recoverableCredit{}
	for rows.Next() {
		var c recoverableCredit
		if err := rows.Scan(&c.EventID, &c.EventTitle, &c.OrderID, &c.AmountCents, &c.Reason, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type markPaidReq struct {
	EventID   uuid.UUID `json:"event_id"`
	Reference string    `json:"reference"` // identificador da transferência (e2e do Pix)
}

// adminMarkPayoutPaid registra que a transferência saiu.
//
// É AÇÃO MANUAL, e de propósito: a execução bancária do repasse não existe no produto —
// nada aqui transfere, saca ou valida titularidade de conta. A referência é o comprovante:
// sem ela, "pago" vira palavra contra palavra na primeira divergência.
func (s *Server) adminMarkPayoutPaid(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
	producerID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body markPaidReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if body.Reference == "" {
		writeErr(w, http.StatusBadRequest, "informe a referência da transferência")
		return
	}
	var ok bool
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		var e error
		ok, e = payout.MarkPaid(r.Context(), tx, body.EventID, body.Reference, claims.Subject)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "repasse não encontrado, já pago ou ainda não vencido")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type holdReq struct {
	EventID uuid.UUID `json:"event_id"`
	Reason  string    `json:"reason"`
}

// adminHoldPayout retém ou solta o repasse de um evento. O motivo vem de uma lista fechada:
// reter dinheiro de alguém por um motivo que não está em lugar nenhum é o tipo de decisão
// que ninguém consegue revisar depois — e o produtor vê o texto correspondente no painel.
func (s *Server) adminHoldPayout(hold bool) func(http.ResponseWriter, *http.Request, *auth.AdminClaims) {
	return func(w http.ResponseWriter, r *http.Request, claims *auth.AdminClaims) {
		producerID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "id inválido")
			return
		}
		var body holdReq
		if err := decode(w, r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "corpo inválido")
			return
		}
		if hold && !payout.ValidHoldReason(body.Reason) {
			writeErr(w, http.StatusBadRequest, "motivo de retenção desconhecido")
			return
		}
		if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
			if hold {
				return payout.Hold(r.Context(), tx, body.EventID, body.Reason, claims.Subject)
			}
			return payout.Release(r.Context(), tx, body.EventID)
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// adminHoldReasons expõe a lista fechada de motivos, com o texto que o produtor lê. O painel
// não inventa a lista do lado dele.
func (s *Server) adminHoldReasons(w http.ResponseWriter, r *http.Request, _ *auth.AdminClaims) {
	type reason struct {
		Key     string `json:"key"`
		Message string `json:"message"`
	}
	list := []reason{}
	for _, k := range []string{payout.HoldEventCancelled, payout.HoldDispute, payout.HoldBankPending, payout.HoldAdminDecision} {
		list = append(list, reason{Key: k, Message: payout.HoldMessage(k)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"reasons": list})
}

type payoutDelayReq struct {
	// EventID vazio edita o PADRÃO da casa; preenchido, a sobrescrita daquele evento.
	EventID *uuid.UUID `json:"event_id"`
	Days    *int       `json:"payout_delay_days"`
}

// adminPayoutDelay lê e grava em quantos dias após a realização o repasse vence.
//
// É parâmetro da PLATAFORMA, não do produtor: quem decide quando o dinheiro sai é quem está
// com ele. Por produtor, com sobrescrita por evento — um evento grande e um teste de fim de
// semana não precisam do mesmo prazo, e chumbar sete dias no código tiraria a conversa da
// mesa.
func (s *Server) adminPayoutDelay(w http.ResponseWriter, r *http.Request, _ *auth.AdminClaims) {
	producerID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	if r.Method == http.MethodGet {
		var days int
		var inherited bool
		eventID := uuid.Nil
		if raw := r.URL.Query().Get("event_id"); raw != "" {
			if eventID, err = uuid.Parse(raw); err != nil {
				writeErr(w, http.StatusBadRequest, "evento inválido")
				return
			}
		}
		if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
			var e error
			days, inherited, e = payout.Delay(r.Context(), tx, eventID)
			return e
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"payout_delay_days": days, "inherited": inherited, "default_days": payout.DefaultDelayDays,
		})
		return
	}

	var body payoutDelayReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if body.Days == nil || *body.Days < 0 {
		writeErr(w, http.StatusBadRequest, "informe payout_delay_days (>= 0)")
		return
	}
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		if e := payout.SetDelay(r.Context(), tx, body.EventID, *body.Days); e != nil {
			return e
		}
		// O prazo entra no vencimento já gravado: mudar a política e deixar as datas
		// antigas de pé faria a tela do produtor mostrar uma promessa que ninguém mais tem.
		ids, e := payout.EventIDs(r.Context(), tx)
		if e != nil {
			return e
		}
		for _, id := range ids {
			if body.EventID != nil && *body.EventID != id {
				continue
			}
			if _, e := payout.Recompute(r.Context(), tx, id); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
