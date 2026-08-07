package api

import (
	"encoding/csv"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/dash"
	"github.com/jadersonmarc/sapienza-timbre/internal/ledger"
)

// dashOverview devolve, em uma chamada, o que o painel do produtor mostra em tempo
// real: curva por lote, ocupação, andamento do check-in e financeiro do evento.
func (s *Server) dashOverview(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var (
		sales []dash.LotSales
		occ   dash.Occupancy
		chk   dash.Checkin
		fin   dash.Finance
	)
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		if sales, e = dash.SalesByLot(r.Context(), tx, eventID); e != nil {
			return e
		}
		if occ, e = dash.EventOccupancy(r.Context(), tx, eventID); e != nil {
			return e
		}
		if chk, e = dash.CheckinProgress(r.Context(), tx, eventID); e != nil {
			return e
		}
		fin, e = dash.EventFinance(r.Context(), tx, eventID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sales": sales, "occupancy": occ, "checkin": chk, "finance": fin,
	})
}

// dashSummary é o resumo do produtor (todos os eventos).
func (s *Server) dashSummary(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	var sum dash.ProducerSummary
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		sum, e = dash.Summary(r.Context(), tx)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

type payoutRow struct {
	ID           uuid.UUID  `json:"id"`
	AmountCents  int64      `json:"amount_cents"`
	Status       string     `json:"status"`
	ScheduledFor *time.Time `json:"scheduled_for,omitempty"`
	SentAt       *time.Time `json:"sent_at,omitempty"`
}

// dashPayouts mostra o extrato de repasses: o líquido disponível agora e os payouts.
func (s *Server) dashPayouts(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	var netDue int64
	var payouts []payoutRow
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		if netDue, e = ledger.NetDue(r.Context(), tx); e != nil {
			return e
		}
		rows, e := tx.Query(r.Context(), `SELECT id, amount_cents, status, scheduled_for, sent_at FROM payouts ORDER BY created_at DESC`)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var p payoutRow
			if e := rows.Scan(&p.ID, &p.AmountCents, &p.Status, &p.ScheduledFor, &p.SentAt); e != nil {
				return e
			}
			payouts = append(payouts, p)
		}
		return rows.Err()
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"net_due_cents": netDue, "payouts": payouts})
}

// dashExportCSV exporta os ingressos do evento em CSV.
func (s *Server) dashExportCSV(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var tickets []dash.TicketRow
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		tickets, e = dash.TicketsForExport(r.Context(), tx, eventID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=ingressos.csv")
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"ticket_id", "lote", "assento", "status", "emitido_em"})
	for _, t := range tickets {
		_ = cw.Write([]string{t.TicketID, t.LotName, t.Seat, t.Status, t.Created})
	}
	cw.Flush()
}
