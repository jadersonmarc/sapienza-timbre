package api

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/catalog"
)

var validSectorKind = map[string]bool{"standing": true, "seated": true, "table": true}

type createSectorReq struct {
	Name        string          `json:"name"`
	Kind        string          `json:"kind"`
	SellUnit    *string         `json:"sell_unit"`
	Rows        *int            `json:"rows"`
	SeatsPerRow *int            `json:"seats_per_row"`
	NamingRule  json.RawMessage `json:"naming_rule"`
}

func (s *Server) createSector(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body createSectorReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if body.Name == "" || !validSectorKind[body.Kind] {
		writeErr(w, http.StatusBadRequest, "name e kind (standing|seated|table) obrigatórios")
		return
	}
	var out catalog.Sector
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = catalog.CreateSector(r.Context(), tx, catalog.Sector{
			EventID: eventID, Name: body.Name, Kind: body.Kind, SellUnit: body.SellUnit,
			Rows: body.Rows, SeatsPerRow: body.SeatsPerRow, NamingRule: body.NamingRule,
		})
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) listSectors(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var out []catalog.Sector
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = catalog.ListSectors(r.Context(), tx, eventID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sectors": out})
}

type generateSeatsReq struct {
	Rows        int    `json:"rows"`
	SeatsPerRow int    `json:"seats_per_row"`
	RowStyle    string `json:"row_style"` // alpha | numeric
}

func (s *Server) generateSeats(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	sectorID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body generateSeatsReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if body.RowStyle == "" {
		body.RowStyle = "alpha"
	}
	var created int
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		created, e = catalog.GenerateSeats(r.Context(), tx, sectorID, body.Rows, body.SeatsPerRow, body.RowStyle)
		return e
	}); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"created": created})
}

func (s *Server) listSeats(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	sectorID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var out []catalog.Seat
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = catalog.ListSeats(r.Context(), tx, sectorID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"seats": out})
}

type blockSeatReq struct {
	Reason string `json:"reason"` // vazio = desbloquear
}

func (s *Server) blockSeat(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	seatID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body blockSeatReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		return catalog.BlockSeat(r.Context(), tx, seatID, body.Reason)
	}); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type setPriceReq struct {
	SectorID       string `json:"sector_id"`
	PriceCents     int64  `json:"price_cents"`
	HalfPriceCents *int64 `json:"half_price_cents"`
}

func (s *Server) setSectorPrice(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	lotID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body setPriceReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	sectorID, err := uuid.Parse(body.SectorID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "sector_id inválido")
		return
	}
	var out catalog.SectorPrice
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = catalog.SetSectorPrice(r.Context(), tx, catalog.SectorPrice{
			LotID: lotID, SectorID: sectorID, PriceCents: body.PriceCents, HalfPriceCents: body.HalfPriceCents,
		})
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listSectorPrices(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	lotID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var out []catalog.SectorPrice
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		out, e = catalog.ListSectorPrices(r.Context(), tx, lotID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"prices": out})
}
