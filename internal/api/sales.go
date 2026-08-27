package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
)

// saleRow é uma venda como quem atende precisa ver: quem comprou, quanto, em que pé está e
// quantos ingressos ainda valem. O painel só tinha agregados — e ninguém atende reclamação
// com agregado.
type saleRow struct {
	OrderID     uuid.UUID  `json:"order_id"`
	EventID     uuid.UUID  `json:"event_id"`
	EventTitle  string     `json:"event_title"`
	BuyerName   string     `json:"buyer_name"`
	BuyerEmail  string     `json:"buyer_email"`
	BuyerCPF    string     `json:"buyer_cpf"`
	Status      string     `json:"status"`
	Method      string     `json:"method"`
	TotalCents  int64      `json:"total_cents"`
	FaceCents   int64      `json:"face_cents"`
	Tickets     int        `json:"tickets"`
	ActiveCount int        `json:"active_tickets"`
	CreatedAt   time.Time  `json:"created_at"`
	PaidAt      *time.Time `json:"paid_at"`
	// RefundRequestStatus é o pedido de estorno vivo desta compra, se houver. É a primeira
	// coisa que quem atende precisa saber antes de prometer qualquer coisa.
	RefundRequestID     *uuid.UUID `json:"refund_request_id"`
	RefundRequestStatus *string    `json:"refund_request_status"`
}

// searchSales procura vendas por comprador (nome, e-mail, CPF), por id do pedido ou por id
// do ingresso. Sem `q`, lista as mais recentes; com `event`, restringe ao evento.
//
// Busca por PESSOA é o caminho real do atendimento: quem liga não sabe o id do pedido, sabe
// o próprio e-mail. Busca por ingresso resolve o caso da portaria.
func (s *Server) searchSales(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var eventID *uuid.UUID
	if raw := r.URL.Query().Get("event"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "evento inválido")
			return
		}
		eventID = &id
	}
	var list []saleRow
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		list, e = querySales(r.Context(), tx, q, eventID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []saleRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sales": list})
}

func querySales(ctx context.Context, tx pgx.Tx, q string, eventID *uuid.UUID) ([]saleRow, error) {
	// Um id colado na busca pode ser do pedido ou do ingresso — tentar os dois é mais
	// barato que obrigar quem atende a saber qual é qual.
	var asUUID *uuid.UUID
	if id, err := uuid.Parse(q); err == nil {
		asUUID = &id
	}
	rows, err := tx.Query(ctx, `
		SELECT o.id, o.event_id, e.title,
		       COALESCE(o.attendees->0->>'name',''), COALESCE(o.buyer_email,''), COALESCE(o.buyer_cpf,''),
		       o.status, COALESCE(p.method,''), o.total_cents, o.face_cents,
		       (SELECT count(*) FROM tickets t WHERE t.order_id = o.id),
		       (SELECT count(*) FROM tickets t WHERE t.order_id = o.id AND t.status='active'),
		       o.created_at, p.settled_at,
		       rr.id, rr.status
		  FROM orders o
		  JOIN events e ON e.id = o.event_id
		  LEFT JOIN LATERAL (
		        SELECT method, settled_at FROM payments WHERE order_id = o.id
		         ORDER BY created_at DESC LIMIT 1) p ON true
		  LEFT JOIN LATERAL (
		        SELECT id, status FROM refund_requests
		         WHERE order_id = o.id AND status IN ('pending','approved','processing')
		         LIMIT 1) rr ON true
		 WHERE o.status <> 'pending'
		   AND ($1::uuid IS NULL OR o.event_id = $1)
		   AND ($2 = '' OR (
		         o.buyer_email ILIKE '%' || $2 || '%'
		      OR o.attendees::text ILIKE '%' || $2 || '%'
		      OR ($3 <> '' AND regexp_replace(COALESCE(o.buyer_cpf,''), '\D', '', 'g') = $3)
		      OR o.id = $4::uuid
		      OR EXISTS (SELECT 1 FROM tickets t WHERE t.order_id = o.id AND t.id = $4::uuid)))
		 ORDER BY o.created_at DESC
		 LIMIT 100`, eventID, q, digitsOnly.ReplaceAllString(q, ""), asUUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []saleRow
	for rows.Next() {
		var s saleRow
		if err := rows.Scan(&s.OrderID, &s.EventID, &s.EventTitle, &s.BuyerName, &s.BuyerEmail,
			&s.BuyerCPF, &s.Status, &s.Method, &s.TotalCents, &s.FaceCents, &s.Tickets,
			&s.ActiveCount, &s.CreatedAt, &s.PaidAt, &s.RefundRequestID, &s.RefundRequestStatus); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// saleTickets lista os ingressos de uma venda, com o estado de cada um. É o que a tela
// precisa para o estorno PARCIAL: sem saber quais ingressos existem, só dá para devolver
// tudo.
func (s *Server) saleTickets(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	orderID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "pedido inválido")
		return
	}
	type ticketRow struct {
		ID           uuid.UUID `json:"id"`
		Status       string    `json:"status"`
		AttendeeName string    `json:"attendee_name"`
		HalfPrice    bool      `json:"half_price"`
		Sector       string    `json:"sector"`
		Seat         string    `json:"seat"`
		CheckedIn    bool      `json:"checked_in"`
	}
	var list []ticketRow
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		rows, e := tx.Query(r.Context(), `
			SELECT t.id, t.status, COALESCE(t.attendee_name,''), t.half_price,
			       COALESCE(sec.name,''), COALESCE(se.row_label || se.number, ''),
			       EXISTS (SELECT 1 FROM checkins c WHERE c.ticket_id = t.id AND NOT c.is_reentry)
			  FROM tickets t
			  LEFT JOIN seats se  ON se.id = t.seat_id
			  LEFT JOIN sectors sec ON sec.id = se.sector_id
			 WHERE t.order_id = $1
			 ORDER BY t.created_at, t.id`, orderID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var t ticketRow
			if e := rows.Scan(&t.ID, &t.Status, &t.AttendeeName, &t.HalfPrice, &t.Sector, &t.Seat, &t.CheckedIn); e != nil {
				return e
			}
			list = append(list, t)
		}
		return rows.Err()
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []ticketRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tickets": list})
}

// adminSearchSales é a mesma busca, varrendo TODOS os produtores. É o que a plataforma usa
// quando alguém escreve para ela sem saber de qual casa comprou — que é o caso normal.
func (s *Server) adminSearchSales(w http.ResponseWriter, r *http.Request, _ *auth.AdminClaims) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeErr(w, http.StatusBadRequest, "informe o que procurar (e-mail, CPF, nome, id do pedido ou do ingresso)")
		return
	}
	type adminSale struct {
		saleRow
		ProducerID   uuid.UUID `json:"producer_id"`
		ProducerName string    `json:"producer_name"`
	}
	out := []adminSale{}
	// O índice público responde onde procurar sem varrer schema quando a busca é por
	// e-mail; nos demais casos não há atalho, e varrer é o preço de achar.
	producers, err := listProducerRefs(r.Context(), s)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, p := range producers {
		var list []saleRow
		if err := s.withTenant(r.Context(), p.id, func(tx pgx.Tx) error {
			var e error
			list, e = querySales(r.Context(), tx, q, nil)
			return e
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, row := range list {
			out = append(out, adminSale{saleRow: row, ProducerID: p.id, ProducerName: p.name})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sales": out})
}

type producerRef struct {
	id   uuid.UUID
	name string
}

func listProducerRefs(ctx context.Context, s *Server) ([]producerRef, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name FROM producers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []producerRef
	for rows.Next() {
		var p producerRef
		if err := rows.Scan(&p.id, &p.name); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
