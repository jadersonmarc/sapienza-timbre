package api

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/catalog"
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
	if body.Title == "" || !catalog.ValidCategory(body.Category) {
		writeErr(w, http.StatusBadRequest, "title e category (válida) obrigatórios")
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
			Lat: body.Lat, Lng: body.Lng, Capacity: body.Capacity, AgeRating: body.AgeRating,
			CancellationPolicy: body.CancellationPolicy, Terms: body.Terms, HasSeatMap: body.HasSeatMap,
		})
		return e
	}); err != nil {
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
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		return catalog.PublishEvent(r.Context(), tx, claims.ProducerID, id)
	}); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ── lotes ────────────────────────────────────────────────────────────────────

type createLotReq struct {
	Name       string  `json:"name"`
	PriceCents int64   `json:"price_cents"`
	Stock      int     `json:"stock"`
	StartsAt   *string `json:"starts_at"`
	EndsAt     *string `json:"ends_at"`
	Position   int     `json:"position"`
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
	if body.Name == "" || body.PriceCents < 0 || body.Stock < 0 {
		writeErr(w, http.StatusBadRequest, "name, price_cents e stock obrigatórios (>= 0)")
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
			EventID: eventID, Name: body.Name, PriceCents: body.PriceCents, Stock: body.Stock,
			StartsAt: starts, EndsAt: ends, Position: body.Position,
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
