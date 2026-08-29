// Package gate é a portaria: validação do ingresso (assinatura Ed25519 verificável
// OFFLINE), registro de check-in com auditoria e reconciliação idempotente do que foi
// escaneado sem sinal. A prevenção de duplicidade é do schema (índice
// checkins_primary_admission_key), não da aplicação.
package gate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// Attendee é o nome de quem deveria estar usando o ingresso — é o que a portaria
	// confere com o documento. Vazio em ingresso emitido antes da ficha nominal, e nesse
	// caso a entrada segue valendo pelo QR.
	Attendee string `json:"attendee,omitempty"`
	// HalfPrice avisa que essa entrada exige comprovação de meia na porta.
	HalfPrice bool `json:"half_price,omitempty"`
}

// ValidateToken confere a assinatura (offline, sem banco) e devolve o payload.
func ValidateToken(v *ticketing.Verifier, token string) (ticketing.Payload, error) {
	return v.Verify(token)
}

// Checkin valida o token, registra a entrada e devolve o veredito. Idempotente por
// client_uid. Deve rodar sob tenancy.WithTenant.
func Checkin(ctx context.Context, tx pgx.Tx, v *ticketing.Verifier, producerID uuid.UUID, in Input) (Result, error) {
	payload, err := ValidateToken(v, in.Token)
	if err != nil {
		return Result{Verdict: Invalid}, nil
	}
	res := Result{TicketID: payload.TicketID}

	// Ingresso precisa existir e estar ativo (não queimado/cancelado).
	var status string
	var eventID uuid.UUID
	var seatID, ownerSubject, ownerWallet *uuid.UUID
	var attendee *string
	var halfPrice bool
	err = tx.QueryRow(ctx, `
		SELECT status, event_id, seat_id, owner_subject_id, owner_wallet_id, attendee_name, half_price
		  FROM tickets WHERE id=$1`, payload.TicketID).
		Scan(&status, &eventID, &seatID, &ownerSubject, &ownerWallet, &attendee, &halfPrice)
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
	// Evento fechado (atestado vigente) não aceita novos check-ins.
	var closed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM event_attestations WHERE event_id=$1 AND supersedes_id IS NULL)`, eventID).Scan(&closed); err != nil {
		return Result{}, err
	}
	if closed {
		res.Verdict = Invalid
		return res, nil
	}
	if attendee != nil {
		res.Attendee = *attendee
	}
	res.HalfPrice = halfPrice
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
	var checkinID uuid.UUID
	insErr := sp.QueryRow(ctx, `
		INSERT INTO checkins (ticket_id, gate, operator, is_reentry, device_id, client_uid, entered_at, synced_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7, now()) RETURNING id`,
		payload.TicketID, nilStr(in.Gate), nilStr(in.Operator), in.Reentry, nilStr(in.DeviceID), nilStr(in.ClientUID), enteredAt).Scan(&checkinID)
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

	// Registro de presença (intransferível, gratuito) na admissão primária. O vínculo é
	// ao sujeito do público (do ingresso, ou via carteira). Idempotente por ingresso.
	if !in.Reentry {
		subject := ownerSubject
		if subject == nil && ownerWallet != nil {
			var sid uuid.UUID
			if e := tx.QueryRow(ctx, `SELECT subject_id FROM public.wallets WHERE id=$1`, *ownerWallet).Scan(&sid); e == nil {
				subject = &sid
			}
		}
		// Snapshot do evento (título/geo) para o panorama montar mapa e linha do tempo.
		var evTitle *string
		var lat, lng *float64
		_ = tx.QueryRow(ctx, `SELECT title, lat, lng FROM events WHERE id=$1`, payload.EventID).Scan(&evTitle, &lat, &lng)
		if _, err := tx.Exec(ctx, `
			INSERT INTO public.attendance_records (subject_id, producer_id, event_id, ticket_id, checkin_id, gate, occurred_at, event_title, venue_lat, venue_lng)
			VALUES ($1,$2,$3,$4,$5,$6, now(), $7,$8,$9)
			ON CONFLICT (ticket_id) WHERE ticket_id IS NOT NULL DO NOTHING`,
			subject, producerID, payload.EventID, payload.TicketID, checkinID, nilStr(in.Gate), evTitle, lat, lng); err != nil {
			return Result{}, err
		}
	}
	res.Verdict = verdictFor(in.Reentry)
	return res, nil
}

// Sync reconcilia um lote de scans feitos offline. Ordem preservada nos resultados.
func Sync(ctx context.Context, tx pgx.Tx, v *ticketing.Verifier, producerID uuid.UUID, items []Input) ([]Result, error) {
	out := make([]Result, 0, len(items))
	for _, in := range items {
		r, err := Checkin(ctx, tx, v, producerID, in)
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

// ── aparelhos da portaria ────────────────────────────────────────────────────

// Device é um aparelho que já sincronizou com o servidor.
type Device struct {
	DeviceID       string    `json:"device_id"`
	KeyFingerprint string    `json:"key_fingerprint"`
	Gate           string    `json:"gate"`
	Operator       string    `json:"operator"`
	CheckinsSynced int64     `json:"checkins_synced"`
	FirstSeenAt    time.Time `json:"first_seen_at"`
	LastSyncAt     time.Time `json:"last_sync_at"`
	KeyCurrent     bool      `json:"key_current"`
}

// KeyFingerprint reduz a chave pública a uma impressão curta e comparável.
//
// Sobre o base64 exatamente como ele trafega, e não sobre os bytes decodificados: assim a
// portaria calcula a mesma coisa no navegador, sem precisar decodificar nada para conferir
// se está com a chave certa.
func KeyFingerprint(publicKeyB64 string) string {
	if publicKeyB64 == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(publicKeyB64))
	return hex.EncodeToString(sum[:])[:12]
}

// TouchDevice registra a passagem do aparelho: quando falou, com qual chave e quantos
// check-ins entregou. Roda na mesma transação do sync — aparelho que não conseguiu
// entregar nada não vira "sincronizado agora".
func TouchDevice(ctx context.Context, tx pgx.Tx, deviceID, fingerprint, gate, operator string, synced int) error {
	if deviceID == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO gate_devices (device_id, key_fingerprint, last_gate, last_operator, checkins_synced)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (device_id) DO UPDATE SET
			key_fingerprint = COALESCE(NULLIF(EXCLUDED.key_fingerprint,''), gate_devices.key_fingerprint),
			last_gate       = COALESCE(NULLIF(EXCLUDED.last_gate,''), gate_devices.last_gate),
			last_operator   = COALESCE(NULLIF(EXCLUDED.last_operator,''), gate_devices.last_operator),
			checkins_synced = gate_devices.checkins_synced + EXCLUDED.checkins_synced,
			last_sync_at    = now()`,
		deviceID, nilIfEmpty(fingerprint), nilIfEmpty(gate), nilIfEmpty(operator), synced)
	return err
}

// ListDevices lista os aparelhos, do que sincronizou há mais tempo para o mais recente —
// o de cima é o que tem mais chance de estar desatualizado na hora da porta.
func ListDevices(ctx context.Context, tx pgx.Tx, currentFingerprint string) ([]Device, error) {
	rows, err := tx.Query(ctx, `
		SELECT device_id, COALESCE(key_fingerprint,''), COALESCE(last_gate,''),
		       COALESCE(last_operator,''), checkins_synced, first_seen_at, last_sync_at
		  FROM gate_devices ORDER BY last_sync_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.DeviceID, &d.KeyFingerprint, &d.Gate, &d.Operator,
			&d.CheckinsSynced, &d.FirstSeenAt, &d.LastSyncAt); err != nil {
			return nil, err
		}
		// Impressão vazia = aparelho de uma versão anterior do app, que ainda não informa a
		// chave. Não é "está com a chave certa": é "não sabemos".
		d.KeyCurrent = d.KeyFingerprint != "" && d.KeyFingerprint == currentFingerprint
		out = append(out, d)
	}
	return out, rows.Err()
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
