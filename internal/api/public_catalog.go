package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/attest"
	"github.com/jadersonmarc/sapienza-timbre/internal/catalog"
	"github.com/jadersonmarc/sapienza-timbre/internal/checkout"
	"github.com/jadersonmarc/sapienza-timbre/internal/inventory"
	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
)

// Página do diretório — teto de itens por página (evita varredura cara).
const publicPageSize = 24

// eventCard é o DTO PÚBLICO do card do diretório (§4.1: DTO explícito, nada de owner).
type eventCard struct {
	EventID       uuid.UUID  `json:"event_id"`
	Title         string     `json:"title"`
	Category      *string    `json:"category,omitempty"`
	City          *string    `json:"city,omitempty"`
	StartsAt      *time.Time `json:"starts_at,omitempty"`
	CoverURL      *string    `json:"cover_url,omitempty"`
	MinPriceCents *int64     `json:"min_price_cents,omitempty"`
}

// listPublicEvents é o diretório público: busca por termo, categoria, cidade e janela de
// datas, sobre o event_directory (só eventos publicados). Paginado.
func (s *Server) listPublicEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT event_id, title, category, city, starts_at, cover_url, min_price_cents
		  FROM event_directory
		 WHERE ($1 = '' OR title ILIKE '%'||$1||'%')
		   AND ($2 = '' OR category = $2)
		   AND ($3 = '' OR city ILIKE '%'||$3||'%')
		   AND ($4::timestamptz IS NULL OR starts_at >= $4)
		   AND ($5::timestamptz IS NULL OR starts_at <= $5)
		 ORDER BY starts_at NULLS LAST, title
		 LIMIT $6 OFFSET $7`,
		q.Get("q"), q.Get("category"), q.Get("city"),
		nilIfEmptyTime(q.Get("from")), nilIfEmptyTime(q.Get("to")),
		publicPageSize, (page-1)*publicPageSize)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	out := []eventCard{}
	for rows.Next() {
		var c eventCard
		if err := rows.Scan(&c.EventID, &c.Title, &c.Category, &c.City, &c.StartsAt, &c.CoverURL, &c.MinPriceCents); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out, "page": page, "page_size": publicPageSize})
}

// listPublicCategories devolve as categorias efetivamente navegáveis (presentes no
// diretório), com contagem de eventos publicados.
func (s *Server) listPublicCategories(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT category, count(*) FROM event_directory
		 WHERE category IS NOT NULL GROUP BY category ORDER BY category`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	type cat struct {
		Slug  string `json:"slug"`
		Count int    `json:"count"`
	}
	out := []cat{}
	for rows.Next() {
		var c cat
		if err := rows.Scan(&c.Slug, &c.Count); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": out})
}

// publicEventDetail é o DTO público da página do evento (§4.1: sem campo de owner). Os
// contadores do lote (saldo) entram porque a página os mostra; o web trata como voláteis.
type publicEventDetail struct {
	Event   publicEvent    `json:"event"`
	Lots    []publicLot    `json:"lots"`
	Current *uuid.UUID     `json:"current_lot_id,omitempty"`
	Sectors []publicSector `json:"sectors"`
	// Producer é quem apresenta o evento. O comprador precisa saber de quem está comprando
	// antes de pagar, e é isso que a reputação da casa passa a significar alguma coisa.
	Producer publicProducer `json:"producer"`
	// HalfPrice diz se ainda cabe meia-entrada e qual a cota do evento. A Lei 12.933/2013,
	// art. 1º, §1º, obriga a informar isso de forma visível em todos os pontos de venda —
	// não é enfeite de tela, é obrigação.
	HalfPrice halfPriceInfo `json:"half_price"`
}

type publicProducer struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// halfPriceInfo é o que a Lei 12.933/2013 manda expor: o TOTAL de meia do evento e quanto
// ainda há. O vendido e o estoque de inteira continuam de fora.
type halfPriceInfo struct {
	Available bool `json:"available"`
	Quota     int  `json:"quota"`
	Granted   int  `json:"granted"`
	Remaining int  `json:"remaining"`
	// Mode diz se a meia tem cota própria ('quota') ou segue o estoque do lote pai
	// ('linked'). Sem isso, "quota igual à capacidade" pareceria número inventado.
	Mode string `json:"mode"`
}

type publicEvent struct {
	ID       uuid.UUID `json:"id"`
	Title    string    `json:"title"`
	Subtitle *string   `json:"subtitle,omitempty"`
	// Description vem em TEXTO com marcação simples. A página renderiza a partir de um
	// conjunto fechado de elementos — nunca injeta o texto como HTML.
	Description *string    `json:"description,omitempty"`
	Category    string     `json:"category"`
	CoverURL    *string    `json:"cover_url,omitempty"`
	StartsAt    *time.Time `json:"starts_at,omitempty"`
	EndsAt      *time.Time `json:"ends_at,omitempty"`
	Address     *string    `json:"address,omitempty"`
	// VenueName é o nome do lugar — é ele que o comprador reconhece.
	VenueName          *string  `json:"venue_name,omitempty"`
	City               *string  `json:"city,omitempty"`
	Lat                *float64 `json:"lat,omitempty"`
	Lng                *float64 `json:"lng,omitempty"`
	AgeRating          *string  `json:"age_rating,omitempty"`
	CancellationPolicy *string  `json:"cancellation_policy,omitempty"`
	Terms              *string  `json:"terms,omitempty"`
	HasSeatMap         bool     `json:"has_seat_map"`
}

type publicLot struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	PriceCents int64      `json:"price_cents"`
	Available  int        `json:"available"`
	StartsAt   *time.Time `json:"starts_at,omitempty"`
	EndsAt     *time.Time `json:"ends_at,omitempty"`
	SortOrder  int        `json:"sort_order"`
	// Faixa de quantidade por compra: o seletor da página trava nela, e o preço acima é o
	// unitário — o total mostrado é preço × quantidade.
	MinPurchaseQuantity int  `json:"min_purchase_quantity"`
	MaxPurchaseQuantity *int `json:"max_purchase_quantity,omitempty"`
	// Notice é o aviso desta categoria, já sanitizado na escrita: a página o renderiza
	// como TEXTO, nunca como HTML.
	Notice *string `json:"notice,omitempty"`
	// OnSale diz se ESTE lote pode ser comprado agora. Com lotes simultâneos não há um
	// vigente só, e a página precisa distinguir "à venda" de "ainda vai abrir" sem deduzir
	// a fila do lado dela.
	OnSale bool `json:"on_sale"`
}

type publicSector struct {
	ID     uuid.UUID     `json:"id"`
	Name   string        `json:"name"`
	Kind   string        `json:"kind"`
	Prices []publicPrice `json:"prices,omitempty"`
	Seats  []publicSeat  `json:"seats,omitempty"`
}

type publicPrice struct {
	LotID          uuid.UUID `json:"lot_id"`
	PriceCents     int64     `json:"price_cents"`
	HalfPriceCents *int64    `json:"half_price_cents,omitempty"`
}

type publicSeat struct {
	ID       uuid.UUID `json:"id"`
	RowLabel *string   `json:"row_label,omitempty"`
	Number   *string   `json:"number,omitempty"`
	Blocked  bool      `json:"blocked"`
}

// getPublicEvent monta a página do evento resolvendo o produtor pelo diretório e lendo o
// tenant. Só evento publicado (o diretório só contém publicados).
func (s *Server) getPublicEvent(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	producerID, err := s.producerOfEvent(r.Context(), eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "evento não encontrado")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var detail publicEventDetail
	// Slice vazio (nunca null no JSON): o web faz .some/.find direto e quebraria com null.
	detail.Lots = []publicLot{}
	detail.Sectors = []publicSector{}
	err = s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		ev, e := catalog.GetEvent(r.Context(), tx, eventID)
		if e != nil {
			return e
		}
		detail.Event = toPublicEvent(ev)
		lots, e := catalog.ListLots(r.Context(), tx, eventID)
		if e != nil {
			return e
		}
		// Quais estão à venda AGORA: o topo da fila mais todos os independentes. Com lotes
		// simultâneos a lista deixa de ter um vencedor só, e é o comprador que escolhe.
		abertos, e := catalog.AvailableLots(r.Context(), tx, eventID)
		if e != nil {
			return e
		}
		aberto := map[uuid.UUID]bool{}
		for _, l := range abertos {
			aberto[l.ID] = true
		}
		for _, l := range lots {
			// Categoria oculta não aparece na página: ela existe só para quem tem o link.
			if l.Hidden {
				continue
			}
			detail.Lots = append(detail.Lots, publicLot{
				ID: l.ID, Name: l.Name, PriceCents: l.PriceCents,
				Available: l.Quantity - l.SoldCount - l.HeldCount,
				StartsAt:  l.StartsAt, EndsAt: l.EndsAt, SortOrder: l.SortOrder,
				MinPurchaseQuantity: l.MinPurchaseQuantity, MaxPurchaseQuantity: l.MaxPurchaseQuantity,
				Notice: l.Notice, OnSale: aberto[l.ID],
			})
		}
		// Quem apresenta o evento (fora do tenant: o produtor mora em public).
		detail.Producer = publicProducer{ID: producerID}
		// Cota de meia: quando acaba, a meia sai de venda e a inteira continua.
		if hp, e := attest.HalfPrice(r.Context(), tx, eventID); e == nil {
			detail.HalfPrice = halfPriceInfo{
				Available: hp.Available(), Quota: hp.Quota, Granted: hp.Granted,
				Remaining: hp.Remaining, Mode: hp.Mode,
			}
		} else {
			return e
		}
		if len(abertos) > 0 {
			detail.Current = &abertos[0].ID
		}
		sectors, e := catalog.ListSectors(r.Context(), tx, eventID)
		if e != nil {
			return e
		}
		for _, sec := range sectors {
			ps := publicSector{ID: sec.ID, Name: sec.Name, Kind: sec.Kind}
			seats, e := catalog.ListSeats(r.Context(), tx, sec.ID)
			if e != nil {
				return e
			}
			for _, st := range seats {
				ps.Seats = append(ps.Seats, publicSeat{ID: st.ID, RowLabel: st.RowLabel, Number: st.Number, Blocked: st.BlockedReason != nil})
			}
			detail.Sectors = append(detail.Sectors, ps)
		}
		// Preços por lote×setor.
		for i := range detail.Sectors {
			for _, l := range lots {
				prices, e := catalog.ListSectorPrices(r.Context(), tx, l.ID)
				if e != nil {
					return e
				}
				for _, p := range prices {
					if p.SectorID == detail.Sectors[i].ID {
						detail.Sectors[i].Prices = append(detail.Sectors[i].Prices, publicPrice{LotID: p.LotID, PriceCents: p.PriceCents, HalfPriceCents: p.HalfPriceCents})
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// O produtor mora em `public`, fora do schema do tenant — por isso a leitura é pelo pool
	// e não dentro da transação acima.
	_ = s.pool.QueryRow(r.Context(), `SELECT name FROM producers WHERE id=$1`, producerID).
		Scan(&detail.Producer.Name)
	writeJSON(w, http.StatusOK, detail)
}

func toPublicEvent(e catalog.Event) publicEvent {
	return publicEvent{
		ID: e.ID, Title: e.Title, Subtitle: e.Subtitle, Description: e.Description, Category: e.Category,
		CoverURL: e.CoverURL, StartsAt: e.StartsAt, EndsAt: e.EndsAt, Address: e.Address,
		VenueName: e.VenueName, City: e.City, Lat: e.Lat, Lng: e.Lng, AgeRating: e.AgeRating,
		CancellationPolicy: e.CancellationPolicy, Terms: e.Terms, HasSeatMap: e.HasSeatMap,
	}
}

// eventOccupancy devolve os assentos OCUPADOS (hold vivo ou ingresso) de um evento — o
// estado volátil que o mapa de assentos consome no cliente (§3.7/§4.2). Não expõe quem
// ocupa, só o id do assento.
func (s *Server) eventOccupancy(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	producerID, err := s.producerOfEvent(r.Context(), eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "evento não encontrado")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	occupied := []string{}
	err = s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		rows, e := tx.Query(r.Context(), `
			SELECT seat_id FROM seat_occupancy
			 WHERE event_id=$1 AND NOT released AND seat_id IS NOT NULL`, eventID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if e := rows.Scan(&id); e != nil {
				return e
			}
			occupied = append(occupied, id.String())
		}
		return rows.Err()
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"occupied": occupied})
}

// publicConfig informa as capacidades LIGADAS ao web (§3.11: a interface só mostra o que
// existe). payment_methods segue o gateway configurado; hold_ttl vem do motor de reserva.
func (s *Server) publicConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"payment_methods":  []string{payment.MethodPix, payment.MethodCard},
		"hold_ttl_seconds": int(inventory.DefaultTTL.Seconds()),
		// Regras do parcelamento vêm do servidor para a tela não repetir o critério (e
		// divergir dele quando mudar).
		"max_installments":      checkout.MaxInstallments,
		"min_installment_cents": checkout.MinInstallmentCents,
	})
}

// checkoutStatus é a espera ativa do Pix: resolve o produtor pelo payment_index (por
// order_id) e devolve o status da ordem. Sem dado sensível.
func (s *Server) checkoutStatus(w http.ResponseWriter, r *http.Request) {
	orderID, err := uuid.Parse(r.PathValue("orderId"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var producerID uuid.UUID
	err = s.pool.QueryRow(r.Context(), `SELECT producer_id FROM payment_index WHERE order_id=$1 LIMIT 1`, orderID).Scan(&producerID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "pedido não encontrado")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var status string
	err = s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `SELECT status FROM orders WHERE id=$1`, orderID).Scan(&status)
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order_id": orderID, "status": status})
}

// myTickets lista os ingressos do comprador autenticado — ESCOPADO pelo subject do token
// (§4.1 IDOR), lendo o índice público (sem varrer schemas). Inclui o token do QR.
func (s *Server) myTickets(w http.ResponseWriter, r *http.Request, subjectID uuid.UUID) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT event_id, event_title, event_starts_at, venue_city, ticket_id, token, seat_label, status, chain_status
		  FROM ticket_directory WHERE subject_id = $1 ORDER BY event_starts_at NULLS LAST, created_at DESC`, subjectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	type ticket struct {
		EventID     uuid.UUID  `json:"event_id"`
		Title       string     `json:"event_title"`
		StartsAt    *time.Time `json:"event_starts_at,omitempty"`
		City        *string    `json:"venue_city,omitempty"`
		TicketID    uuid.UUID  `json:"ticket_id"`
		Token       *string    `json:"token,omitempty"`
		SeatLabel   *string    `json:"seat_label,omitempty"`
		Status      string     `json:"status"`
		ChainStatus string     `json:"chain_status"`
	}
	out := []ticket{}
	for rows.Next() {
		var t ticket
		if err := rows.Scan(&t.EventID, &t.Title, &t.StartsAt, &t.City, &t.TicketID, &t.Token, &t.SeatLabel, &t.Status, &t.ChainStatus); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tickets": out})
}

// nilIfEmptyTime devolve nil para string vazia, senão o RFC3339 parseado (para os filtros
// de data do diretório). Data inválida vira nil (filtro ignorado) — busca não é validação.
func nilIfEmptyTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}
