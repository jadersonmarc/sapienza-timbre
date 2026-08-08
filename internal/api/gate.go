package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/gate"
)

// gateConfig entrega o que o dispositivo da portaria precisa para operar OFFLINE: a
// chave pública que valida os QRs. O mapa de assentos vem por /seatmap.
func (s *Server) gateConfig(w http.ResponseWriter, r *http.Request, _ *auth.Claims) {
	writeJSON(w, http.StatusOK, map[string]any{"public_key": s.signer.PublicKeyB64()})
}

// gateSeatmap devolve o mapa de assentos do evento para exibição offline (setor,
// fileira, número por assento).
func (s *Server) gateSeatmap(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	type seat struct {
		SeatID uuid.UUID `json:"seat_id"`
		Sector string    `json:"sector"`
		Row    string    `json:"row"`
		Number string    `json:"number"`
	}
	var out []seat
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		rows, e := tx.Query(r.Context(), `
			SELECT s.id, COALESCE(se.name,''), COALESCE(s.row_label,''), COALESCE(s.number,'')
			  FROM seats s
			  JOIN sectors se ON se.id = s.sector_id
			 WHERE se.event_id = $1`, eventID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var st seat
			if e := rows.Scan(&st.SeatID, &st.Sector, &st.Row, &st.Number); e != nil {
				return e
			}
			out = append(out, st)
		}
		return rows.Err()
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"seats": out})
}

type gateValidateReq struct {
	Token   string `json:"token"`
	Gate    string `json:"gate"`
	Reentry bool   `json:"reentry"`
}

// gateValidate valida um QR e registra a entrada (modo online).
func (s *Server) gateValidate(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	var body gateValidateReq
	if err := decode(w, r, &body); err != nil || body.Token == "" {
		writeErr(w, http.StatusBadRequest, "token obrigatório")
		return
	}
	var res gate.Result
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		res, e = gate.Checkin(r.Context(), tx, s.signer.Verifier(), claims.ProducerID, gate.Input{
			Token: body.Token, Gate: body.Gate, Operator: claims.Subject, Reentry: body.Reentry,
		})
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type gateSyncItem struct {
	Token     string `json:"token"`
	Gate      string `json:"gate"`
	DeviceID  string `json:"device_id"`
	ClientUID string `json:"client_uid"`
	Reentry   bool   `json:"reentry"`
	EnteredAt string `json:"entered_at"` // RFC3339 do horário no dispositivo
}

type gateSyncReq struct {
	Checkins []gateSyncItem `json:"checkins"`
}

// gateSync reconcilia um lote de check-ins feitos offline. Idempotente por client_uid;
// duplicidades entre dispositivos aparecem como verdict "duplicate".
func (s *Server) gateSync(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	var body gateSyncReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	items := make([]gate.Input, 0, len(body.Checkins))
	for _, c := range body.Checkins {
		enteredAt := time.Time{}
		if c.EnteredAt != "" {
			if t, err := time.Parse(time.RFC3339, c.EnteredAt); err == nil {
				enteredAt = t
			}
		}
		items = append(items, gate.Input{
			Token: c.Token, Gate: c.Gate, Operator: claims.Subject, DeviceID: c.DeviceID,
			ClientUID: c.ClientUID, Reentry: c.Reentry, EnteredAt: enteredAt,
		})
	}
	var results []gate.Result
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		results, e = gate.Sync(r.Context(), tx, s.signer.Verifier(), claims.ProducerID, items)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}
