// Package gate é a portaria: validação do ingresso (assinatura Ed25519 verificável
// OFFLINE), registro de check-in com auditoria e reconciliação idempotente do que foi
// escaneado sem sinal. A prevenção de duplicidade é do schema (índice
// checkins_primary_admission_key), não da aplicação.
package gate

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
)

// Verdict é o resultado de uma tentativa de entrada.
type Verdict string

const (
	Admitted  Verdict = "admitted"  // primeira entrada, ok
	Reentry   Verdict = "reentry"   // reentrada autorizada
	Duplicate Verdict = "duplicate" // já admitido (barrado)
	Invalid   Verdict = "invalid"   // assinatura inválida ou ingresso não-ativo
	Unknown   Verdict = "unknown"   // ingresso inexistente neste evento
)

// SeatInfo é o direcionamento mostrado ao operador (setor/fileira/assento).
type SeatInfo struct {
	Sector string `json:"sector,omitempty"`
	Row    string `json:"row,omitempty"`
	Number string `json:"number,omitempty"`
}

// Input é uma tentativa de entrada. Token é sempre reverificado no servidor (defesa
// em profundidade), mesmo que o dispositivo já tenha validado offline.
type Input struct {
	Token     string
	Gate      string
	Operator  string
	DeviceID  string
	ClientUID string
	Reentry   bool
	EnteredAt time.Time
}

// Result é a resposta por tentativa.
type Result struct {
	TicketID uuid.UUID `json:"ticket_id"`
	Verdict  Verdict   `json:"verdict"`
	Seat     SeatInfo  `json:"seat"`
}

// ValidateToken confere a assinatura (offline, sem banco) e devolve o payload.
func ValidateToken(v *ticketing.Verifier, token string) (ticketing.Payload, error) {
	return v.Verify(token)
}

// Checkin valida o token, registra a entrada e devolve o veredito. Idempotente por
// client_uid. Deve rodar sob tenancy.WithTenant.
func Checkin(ctx context.Context, tx pgx.Tx, v *ticketing.Verifier, in Input) (Result, error) {
	payload, err := ValidateToken(v, in.Token)
	if err != nil {
		return Result{Verdict: Invalid}, nil
	}
	res := Result{TicketID: payload.TicketID}

	// Ingresso precisa existir e estar ativo (não queimado/cancelado).
	var status string
	var seatID *uuid.UUID
	err = tx.QueryRow(ctx, `SELECT status, seat_id FROM tickets WHERE id=$1`, payload.TicketID).Scan(&status, &seatID)
	if errors.Is(err, pgx.ErrNoRows) {
		res.Verdict = Unknown
		return res, nil
	}
	if err != nil {
		return Result{}, err
	}
	if status != "active" {
		res.Verdict = Invalid
		return res, nil
	}
	if seatID != nil {
		res.Seat, err = seatInfo(ctx, tx, *seatID)
		if err != nil {
			return Result{}, err
		}
	}

	// Idempotência: se este scan (client_uid) já foi processado, devolve o mesmo
	// veredito sem inserir de novo.
	if in.ClientUID != "" {
		var reentry bool
		e := tx.QueryRow(ctx, `SELECT is_reentry FROM checkins WHERE client_uid=$1`, in.ClientUID).Scan(&reentry)
		if e == nil {
			res.Verdict = verdictFor(reentry)
			return res, nil
		} else if !errors.Is(e, pgx.ErrNoRows) {
			return Result{}, e
		}
	}

	enteredAt := in.EnteredAt
	if enteredAt.IsZero() {
		enteredAt = time.Now()
	}
	// Savepoint: uma violação de unicidade só desfaz o INSERT, sem abortar a tx do
	// caller (que ainda precisa commitar). Sem isso, o commit externo falharia.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	_, insErr := sp.Exec(ctx, `
		INSERT INTO checkins (ticket_id, gate, operator, is_reentry, device_id, client_uid, entered_at, synced_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7, now())`,
		payload.TicketID, nilStr(in.Gate), nilStr(in.Operator), in.Reentry, nilStr(in.DeviceID), nilStr(in.ClientUID), enteredAt)
	if insErr != nil {
		_ = sp.Rollback(ctx)
		var pgErr *pgconn.PgError
		if errors.As(insErr, &pgErr) && pgErr.Code == "23505" {
			switch pgErr.ConstraintName {
			case "checkins_primary_admission_key":
				// Já houve admissão primária deste ingresso (outro portão/dispositivo).
				res.Verdict = Duplicate
				return res, nil
			case "checkins_client_uid_key":
				// Corrida no mesmo scan: trata como idempotente.
				res.Verdict = verdictFor(in.Reentry)
				return res, nil
			}
		}
		return Result{}, insErr
	}
	if err := sp.Commit(ctx); err != nil {
		return Result{}, err
	}
	res.Verdict = verdictFor(in.Reentry)
	return res, nil
}

// Sync reconcilia um lote de scans feitos offline. Ordem preservada nos resultados.
func Sync(ctx context.Context, tx pgx.Tx, v *ticketing.Verifier, items []Input) ([]Result, error) {
	out := make([]Result, 0, len(items))
	for _, in := range items {
		r, err := Checkin(ctx, tx, v, in)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func verdictFor(reentry bool) Verdict {
	if reentry {
		return Reentry
	}
	return Admitted
}

func seatInfo(ctx context.Context, tx pgx.Tx, seatID uuid.UUID) (SeatInfo, error) {
	var s SeatInfo
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(se.name,''), COALESCE(s.row_label,''), COALESCE(s.number,'')
		  FROM seats s JOIN sectors se ON se.id = s.sector_id
		 WHERE s.id = $1`, seatID).Scan(&s.Sector, &s.Row, &s.Number)
	if errors.Is(err, pgx.ErrNoRows) {
		return SeatInfo{}, nil
	}
	return s, err
}

func nilStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
