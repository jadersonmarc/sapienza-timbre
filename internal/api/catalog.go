package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/catalog"
	"github.com/jadersonmarc/sapienza-timbre/internal/store"
)

// ── eventos ──────────────────────────────────────────────────────────────────

type createEventReq struct {
	Title              string   `json:"title"`
	Description        *string  `json:"description"`
	Category           string   `json:"category"`
	CoverURL           *string  `json:"cover_url"`
	StartsAt           *string  `json:"starts_at"` // RFC3339
	EndsAt             *string  `json:"ends_at"`
	Address            *string  `json:"address"`
	City               *string  `json:"city"`
	Lat                *float64 `json:"lat"`
	Lng                *float64 `json:"lng"`
	Capacity           *int     `json:"capacity"`
	AgeRating          *string  `json:"age_rating"`
	CancellationPolicy *string  `json:"cancellation_policy"`
	Terms              *string  `json:"terms"`
	HasSeatMap         bool     `json:"has_seat_map"`
}

func (s *Server) createEvent(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	var body createEventReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if body.Title == "" || body.Category == "" {
		writeErr(w, http.StatusBadRequest, "title e category obrigatórios")
		return
	}
	starts, err := parseTimePtr(body.StartsAt)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "starts_at inválido (use RFC3339)")
		return
	}
	ends, err := parseTimePtr(body.EndsAt)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "ends_at inválido (use RFC3339)")
		return
	}
	var out catalog.Event
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = catalog.CreateEvent(r.Context(), tx, catalog.Event{
			Title: body.Title, Description: body.Description, Category: body.Category,
			CoverURL: body.CoverURL, StartsAt: starts, EndsAt: ends, Address: body.Address,
			City: body.City, Lat: body.Lat, Lng: body.Lng, Capacity: body.Capacity, AgeRating: body.AgeRating,
			CancellationPolicy: body.CancellationPolicy, Terms: body.Terms, HasSeatMap: body.HasSeatMap,
		})
		return e
	}); err != nil {
		if errors.Is(err, catalog.ErrCategoryInvalid) {
			writeErr(w, http.StatusBadRequest, "categoria inválida")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	var out []catalog.Event
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = catalog.ListEvents(r.Context(), tx)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

func (s *Server) getEvent(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var ev catalog.Event
	var lots []catalog.Lot
	err = s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		if ev, e = catalog.GetEvent(r.Context(), tx, id); e != nil {
			return e
		}
		lots, e = catalog.ListLots(r.Context(), tx, id)
		return e
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "evento não encontrado")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": ev, "lots": lots})
}

func (s *Server) publishEvent(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	// Publicar é abrir a venda: sem conta de recebimento, o dinheiro do produtor não teria
	// para onde ir e a cobrança sairia sem divisão — a compra funcionaria e o repasse
	// simplesmente não aconteceria, em silêncio. Melhor barrar aqui, com o que falta dito.
	prod, err := store.GetProducer(r.Context(), s.pool, claims.ProducerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if prod.AsaasWalletID == nil || *prod.AsaasWalletID == "" {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":         "informe os dados de recebimento antes de publicar",
			"needs_wallet":  true,
			"error_message": "Para vender ingressos precisamos saber para onde enviar o seu dinheiro. Preencha os dados de recebimento no painel — leva um minuto e vale para todos os seus eventos.",
		})
		return
	}
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		return catalog.PublishEvent(r.Context(), tx, claims.ProducerID, id)
	}); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type patchEventReq struct {
	Title              *string `json:"title"`
	Description        *string `json:"description"`
	Category           *string `json:"category"`
	CoverURL           *string `json:"cover_url"`
	StartsAt           *string `json:"starts_at"`
	EndsAt             *string `json:"ends_at"`
	Address            *string `json:"address"`
	City               *string `json:"city"`
	Capacity           *int    `json:"capacity"`
	AgeRating          *string `json:"age_rating"`
	CancellationPolicy *string `json:"cancellation_policy"`
	Terms              *string `json:"terms"`
}

func (s *Server) patchEvent(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body patchEventReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	starts, err := parseTimePtr(body.StartsAt)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "starts_at inválido (use RFC3339)")
		return
	}
	ends, err := parseTimePtr(body.EndsAt)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "ends_at inválido (use RFC3339)")
		return
	}
	var out catalog.Event
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = catalog.PatchEvent(r.Context(), tx, id, catalog.EventPatch{
			Title: body.Title, Description: body.Description, Category: body.Category,
			CoverURL: body.CoverURL, StartsAt: starts, EndsAt: ends, Address: body.Address,
			City: body.City, Capacity: body.Capacity, AgeRating: body.AgeRating,
			CancellationPolicy: body.CancellationPolicy, Terms: body.Terms,
		})
		return e
	}); err != nil {
		switch {
		case errors.Is(err, catalog.ErrCategoryInvalid):
			writeErr(w, http.StatusBadRequest, "categoria inválida")
		case errors.Is(err, catalog.ErrInvalidTransition):
			writeErr(w, http.StatusConflict, err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// transitionEvent devolve um handler que aplica uma transição fixa de ciclo de vida
// (submit-review, suspend, cancel). Publish tem rota própria (com validações).
func (s *Server) transitionEvent(to string) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "id inválido")
			return
		}
		if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
			return catalog.TransitionEvent(r.Context(), tx, id, to)
		}); err != nil {
			if errors.Is(err, catalog.ErrInvalidTransition) {
				writeErr(w, http.StatusConflict, err.Error())
				return
			}
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": to})
	}
}

// listCategories lista as categorias ativas do catálogo.
func (s *Server) listCategories(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	var out []catalog.Category
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = catalog.ListCategories(r.Context(), tx)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": out})
}

// ── lotes ────────────────────────────────────────────────────────────────────

type createLotReq struct {
	Name       string  `json:"name"`
	PriceCents int64   `json:"price_cents"`
	Quantity   int     `json:"quantity"`
	StartsAt   *string `json:"starts_at"`
	EndsAt     *string `json:"ends_at"`
	SortOrder  int     `json:"sort_order"`
}

func (s *Server) createLot(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body createLotReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if body.Name == "" || body.PriceCents < 0 || body.Quantity < 0 {
		writeErr(w, http.StatusBadRequest, "name, price_cents e quantity obrigatórios (>= 0)")
		return
	}
	starts, err := parseTimePtr(body.StartsAt)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "starts_at inválido (use RFC3339)")
		return
	}
	ends, err := parseTimePtr(body.EndsAt)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "ends_at inválido (use RFC3339)")
		return
	}
	var out catalog.Lot
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = catalog.CreateLot(r.Context(), tx, catalog.Lot{
			EventID: eventID, Name: body.Name, PriceCents: body.PriceCents, Quantity: body.Quantity,
			StartsAt: starts, EndsAt: ends, SortOrder: body.SortOrder,
		})
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) listLots(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var out []catalog.Lot
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = catalog.ListLots(r.Context(), tx, eventID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lots": out})
}

// currentLot resolve o lote vigente do evento (virada derivada por data/capacidade).
func (s *Server) currentLot(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var lot catalog.Lot
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		lot, e = catalog.CurrentLot(r.Context(), tx, eventID)
		return e
	}); err != nil {
		if errors.Is(err, catalog.ErrNoCurrentLot) {
			writeErr(w, http.StatusNotFound, "nenhum lote vigente")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, lot)
}

// ── modelos de mapa (venue templates) ────────────────────────────────────────

type venueTemplateReq struct {
	Name   string          `json:"name"`
	Layout json.RawMessage `json:"layout"`
}

func (s *Server) createVenueTemplate(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	var body venueTemplateReq
	if err := decode(w, r, &body); err != nil || body.Name == "" {
		writeErr(w, http.StatusBadRequest, "name obrigatório")
		return
	}
	var out catalog.VenueTemplate
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = catalog.CreateVenueTemplate(r.Context(), tx, body.Name, body.Layout)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) listVenueTemplates(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	var out []catalog.VenueTemplate
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = catalog.ListVenueTemplates(r.Context(), tx)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"venue_templates": out})
}

// ── cupons ───────────────────────────────────────────────────────────────────

type createCouponReq struct {
	Code          string   `json:"code"`
	DiscountPct   *float64 `json:"discount_pct"`
	DiscountCents *int64   `json:"discount_cents"`
	MaxUses       *int     `json:"max_uses"`
	ValidFrom     *string  `json:"valid_from"`
	ValidUntil    *string  `json:"valid_until"`
}

func (s *Server) createCoupon(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body createCouponReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if body.Code == "" {
		writeErr(w, http.StatusBadRequest, "code obrigatório")
		return
	}
	from, err := parseTimePtr(body.ValidFrom)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "valid_from inválido (use RFC3339)")
		return
	}
	until, err := parseTimePtr(body.ValidUntil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "valid_until inválido (use RFC3339)")
		return
	}
	var out catalog.Coupon
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = catalog.CreateCoupon(r.Context(), tx, catalog.Coupon{
			EventID: &eventID, Code: body.Code, DiscountPct: body.DiscountPct,
			DiscountCents: body.DiscountCents, MaxUses: body.MaxUses,
			ValidFrom: from, ValidUntil: until,
		})
		return e
	}); err != nil {
		if isUniqueViolation(err) {
			writeErr(w, http.StatusConflict, "código de cupom já usado neste evento")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) listCoupons(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var out []catalog.Coupon
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = catalog.ListCoupons(r.Context(), tx, eventID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"coupons": out})
}
