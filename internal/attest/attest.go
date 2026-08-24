package attest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/chain"
	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
)

// FormatVersion identifica a versão do formato do registro canônico. Incrementar quando a
// estrutura/serialização mudar.
const FormatVersion = 1

// ── registro canônico (agregado, sem dado pessoal) ──────────────────────────

// Record é o registro canônico do fechamento. A ordem dos campos É a ordem da
// serialização (json.Marshal preserva a ordem do struct) — determinística e documentada.
// Só agregados: nenhum nome, documento, e-mail ou identificador de pessoa.
type Record struct {
	FormatVersion int                `json:"format_version"`
	KeyID         string             `json:"key_id"`
	Event         RecordEvent        `json:"event"`
	Sales         RecordSales        `json:"sales"`
	Courtesy      RecordCourtesy     `json:"courtesy"`
	Attendance    RecordAttendance   `json:"attendance"`
	HalfPrice     RecordHalfPrice    `json:"half_price"`
	Commitments   []RecordCommitment `json:"commitments"`
}

type RecordEvent struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	StartsAt string `json:"starts_at"`
	EndsAt   string `json:"ends_at"`
	City     string `json:"city"`
	Producer string `json:"producer"`
}

type RecordSales struct {
	TicketsSold int                 `json:"tickets_sold"`
	ByLot       []RecordLotSales    `json:"by_lot"`
	BySector    []RecordSectorSales `json:"by_sector"`
}

type RecordLotSales struct {
	LotID string `json:"lot_id"`
	Name  string `json:"name"`
	Sold  int    `json:"sold"`
}

type RecordSectorSales struct {
	SectorID string `json:"sector_id"`
	Name     string `json:"name"`
	Sold     int    `json:"sold"`
}

type RecordCourtesy struct {
	Issued []RecordCategoryCount `json:"issued"`
	Used   []RecordCategoryCount `json:"used"`
}

type RecordCategoryCount struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
}

type RecordAttendance struct {
	Present   int `json:"present"`
	Absent    int `json:"absent"`
	RatePct   int `json:"rate_pct"`
	Reentries int `json:"reentries"`
}

type RecordHalfPrice struct {
	Granted int `json:"granted"`
	Quota   int `json:"quota"`
}

type RecordCommitment struct {
	Kind        string `json:"kind"`
	Category    string `json:"category,omitempty"`
	TargetType  string `json:"target_type"`
	TargetValue string `json:"target_value"`
	Description string `json:"description,omitempty"`
	Realized    string `json:"realized"`
}

// BuildRecord monta o registro canônico a partir do estado do tenant (agregados).
// Exposto para os relatórios provisórios (antes do fechamento).
func BuildRecord(ctx context.Context, tx pgx.Tx, producerID, eventID uuid.UUID) (Record, error) {
	return buildRecord(ctx, tx, producerID, eventID)
}

// buildRecord monta o registro canônico a partir do estado do tenant (agregados).
func buildRecord(ctx context.Context, tx pgx.Tx, producerID, eventID uuid.UUID) (Record, error) {
	rec := Record{FormatVersion: FormatVersion}
	rec.Commitments = []RecordCommitment{}
	rec.Sales.ByLot = []RecordLotSales{}
	rec.Sales.BySector = []RecordSectorSales{}
	rec.Courtesy.Issued = []RecordCategoryCount{}
	rec.Courtesy.Used = []RecordCategoryCount{}

	// identificação do evento
	var startsAt, endsAt, city, producer *string
	var name string
	if err := tx.QueryRow(ctx, `
		SELECT e.title, to_char(e.starts_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		       to_char(e.ends_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), e.city, p.name
		  FROM events e JOIN public.producers p ON p.id = $1
		 WHERE e.id = $2`, producerID, eventID).
		Scan(&name, &startsAt, &endsAt, &city, &producer); err != nil {
		return rec, err
	}
	rec.Event = RecordEvent{
		ID: eventID.String(), Name: name,
		StartsAt: derefStr(startsAt), EndsAt: derefStr(endsAt), City: derefStr(city), Producer: derefStr(producer),
	}

	// vendidos por lote e por setor + total
	lotRows, err := tx.Query(ctx, `
		SELECT l.id, l.name, COUNT(t.id) FROM lots l
		  LEFT JOIN tickets t ON t.lot_id = l.id AND t.status IN ('active','used')
		 WHERE l.event_id = $1 GROUP BY l.id, l.name ORDER BY l.sort_order, l.created_at`, eventID)
	if err != nil {
		return rec, err
	}
	for lotRows.Next() {
		var r RecordLotSales
		if err := lotRows.Scan(&r.LotID, &r.Name, &r.Sold); err != nil {
			lotRows.Close()
			return rec, err
		}
		rec.Sales.ByLot = append(rec.Sales.ByLot, r)
	}
	lotRows.Close()
	if err := lotRows.Err(); err != nil {
		return rec, err
	}

	secRows, err := tx.Query(ctx, `
		SELECT se.id, se.name, COUNT(t.id) FROM sectors se
		  LEFT JOIN seats s ON s.sector_id = se.id
		  LEFT JOIN tickets t ON t.seat_id = s.id AND t.status IN ('active','used')
		 WHERE se.event_id = $1 GROUP BY se.id, se.name ORDER BY se.created_at`, eventID)
	if err != nil {
		return rec, err
	}
	for secRows.Next() {
		var r RecordSectorSales
		if err := secRows.Scan(&r.SectorID, &r.Name, &r.Sold); err != nil {
			secRows.Close()
			return rec, err
		}
		rec.Sales.BySector = append(rec.Sales.BySector, r)
	}
	secRows.Close()
	if err := secRows.Err(); err != nil {
		return rec, err
	}

	var totalTickets int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM tickets WHERE event_id=$1 AND status IN ('active','used')`, eventID).Scan(&totalTickets); err != nil {
		return rec, err
	}
	rec.Sales.TicketsSold = totalTickets

	// cortesias emitidas/utilizadas por categoria
	if err := fillCategoryCounts(ctx, tx, eventID, false, &rec.Courtesy.Issued); err != nil {
		return rec, err
	}
	if err := fillCategoryCounts(ctx, tx, eventID, true, &rec.Courtesy.Used); err != nil {
		return rec, err
	}

	// presença, ausência, taxa, reentradas
	var present, reentries int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(DISTINCT t.id) FROM checkins c JOIN tickets t ON t.id=c.ticket_id
		 WHERE t.event_id=$1 AND NOT c.is_reentry`, eventID).Scan(&present); err != nil {
		return rec, err
	}
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM checkins c JOIN tickets t ON t.id=c.ticket_id
		 WHERE t.event_id=$1 AND c.is_reentry`, eventID).Scan(&reentries); err != nil {
		return rec, err
	}
	absent := totalTickets - present
	if absent < 0 {
		absent = 0
	}
	rate := 0
	if totalTickets > 0 {
		rate = int(math.Round(float64(present) * 100 / float64(totalTickets)))
	}
	rec.Attendance = RecordAttendance{Present: present, Absent: absent, RatePct: rate, Reentries: reentries}

	// meia-entrada
	var granted int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM tickets WHERE event_id=$1 AND half_price AND status IN ('active','used')`, eventID).Scan(&granted); err != nil {
		return rec, err
	}
	quota := halfPriceQuota(ctx, tx, eventID, totalTickets)
	rec.HalfPrice = RecordHalfPrice{Granted: granted, Quota: quota}

	// compromissos declarados + realizado
	comms, err := ListCommitments(ctx, tx, eventID)
	if err != nil {
		return rec, err
	}
	for _, c := range comms {
		realized := realize(ctx, tx, eventID, c, totalTickets)
		catSlug := ""
		if c.CourtesyCategoryID != nil {
			_ = tx.QueryRow(ctx, `SELECT slug FROM courtesy_categories WHERE id=$1`, *c.CourtesyCategoryID).Scan(&catSlug)
		}
		desc := ""
		if c.Description != nil {
			desc = *c.Description
		}
		rec.Commitments = append(rec.Commitments, RecordCommitment{
			Kind: c.Kind, Category: catSlug, TargetType: c.TargetType, TargetValue: c.TargetValue,
			Description: desc, Realized: realized,
		})
	}
	return rec, nil
}

func fillCategoryCounts(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, used bool, out *[]RecordCategoryCount) error {
	pred := `g.status IN ('issued','checked_in')`
	if used {
		pred = `g.status = 'checked_in'`
	}
	rows, err := tx.Query(ctx, `
		SELECT cc.slug, COUNT(g.id) FROM guest_list_entries g
		  JOIN courtesy_categories cc ON cc.id = g.courtesy_category_id
		 WHERE g.event_id=$1 AND `+pred+` GROUP BY cc.slug ORDER BY cc.slug`, eventID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r RecordCategoryCount
		if err := rows.Scan(&r.Category, &r.Count); err != nil {
			return err
		}
		*out = append(*out, r)
	}
	return rows.Err()
}

// halfPriceQuota devolve a cota aplicável de meia-entrada (do compromisso declarado).
func halfPriceQuota(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, total int) int {
	var targetType string
	var targetValue string
	err := tx.QueryRow(ctx, `
		SELECT target_type, target_value::text FROM event_commitments
		 WHERE event_id=$1 AND kind='meia_entrada_cota' ORDER BY created_at LIMIT 1`, eventID).
		Scan(&targetType, &targetValue)
	if err != nil {
		return 0
	}
	v, err := parseValue(targetValue)
	if err != nil {
		return 0
	}
	if targetType == TargetPercent {
		return int(math.Round(float64(total) * v / 100))
	}
	return int(v)
}

// realize computa o realizado de um compromisso, na mesma unidade do target.
func realize(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, c Commitment, total int) string {
	switch c.Kind {
	case KindCourtesyShare:
		var issued int
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM guest_list_entries WHERE event_id=$1 AND courtesy_category_id=$2`,
			eventID, c.CourtesyCategoryID).Scan(&issued); err != nil {
			return ""
		}
		if c.TargetType == TargetPercent && total > 0 {
			return fmt.Sprintf("%d", int(math.Round(float64(issued)*100/float64(total))))
		}
		return fmt.Sprintf("%d", issued)
	case KindMeiaEntradaCota:
		var granted int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM tickets WHERE event_id=$1 AND half_price AND status IN ('active','used')`, eventID).Scan(&granted); err != nil {
			return ""
		}
		return fmt.Sprintf("%d", granted)
	case KindFreeAdmission:
		var issued int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM guest_list_entries WHERE event_id=$1`, eventID).Scan(&issued); err != nil {
			return ""
		}
		return fmt.Sprintf("%d", issued)
	default:
		return ""
	}
}

// SerializeRecord serializa o registro de forma determinística (json.Marshal preserva a
// ordem dos campos do struct). Este é o algoritmo publicado para verificação.
func SerializeRecord(r Record) ([]byte, error) {
	return json.Marshal(r)
}

func serializeRecord(r Record) ([]byte, error) {
	return json.Marshal(r)
}

// digestRecord calcula o resumo SHA-256 sobre a serialização canônica.
func digestRecord(ser []byte) []byte {
	d := sha256.Sum256(ser)
	return d[:]
}

// ── atestado ─────────────────────────────────────────────────────────────────

// Attestation é um atestado fechado do evento.
type Attestation struct {
	ID           uuid.UUID       `json:"id"`
	EventID      uuid.UUID       `json:"event_id"`
	Version      int             `json:"version"`
	SupersedesID *uuid.UUID      `json:"supersedes_id,omitempty"`
	Payload      json.RawMessage `json:"payload"`
	Digest       []byte          `json:"-"`
	Signature    []byte          `json:"-"`
	ClosedAt     time.Time       `json:"closed_at"`
	AnchorStatus string          `json:"anchor_status"`
	AnchorTxHash *string         `json:"anchor_tx_hash,omitempty"`
	AnchoredAt   *time.Time      `json:"anchored_at,omitempty"`
}

// Close fecha o evento: monta o registro canônico, calcula o resumo, assina, persiste e,
// no modo 'log', registra a intenção de ancorar (sem nunca marcar 'anchored'). Idempotente:
// se o atestado vigente já tem o MESMO resumo, devolve o vigente (não refaz). Resumo
// diferente gera NOVA versão (supersedes_id).
func Close(ctx context.Context, tx pgx.Tx, signer *ticketing.Signer, anchorer chain.Anchorer, anchorMode chain.AnchorMode, keyID string, producerID, eventID uuid.UUID) (Attestation, error) {
	rec, err := buildRecord(ctx, tx, producerID, eventID)
	if err != nil {
		return Attestation{}, err
	}
	rec.KeyID = keyID
	ser, err := serializeRecord(rec)
	if err != nil {
		return Attestation{}, err
	}
	digest := digestRecord(ser)
	sig := signer.SignBytes(digest)

	// Atestado vigente?
	var curID *uuid.UUID
	var curVersion int
	var curDigest []byte
	err = tx.QueryRow(ctx, `
		SELECT id, version, digest FROM event_attestations
		 WHERE event_id=$1 AND supersedes_id IS NULL`, eventID).Scan(&curID, &curVersion, &curDigest)
	switch {
	case err == nil && bytes.Equal(curDigest, digest):
		// Já fechado com o mesmo estado: no-op.
		cur, e := loadCurrent(ctx, tx, eventID)
		if e != nil {
			return Attestation{}, e
		}
		return *cur, nil
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return Attestation{}, err
	}

	version := 1
	if curID != nil {
		version = curVersion + 1
	}
	var a Attestation
	a.EventID = eventID
	a.Version = version
	a.SupersedesID = curID
	a.Payload = json.RawMessage(ser)
	a.Digest = digest
	a.Signature = sig
	err = tx.QueryRow(ctx, `
		INSERT INTO event_attestations (event_id, version, supersedes_id, payload, digest, signature)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id, closed_at, anchor_status`,
		a.EventID, a.Version, a.SupersedesID, a.Payload, a.Digest, a.Signature).
		Scan(&a.ID, &a.ClosedAt, &a.AnchorStatus)
	if err != nil {
		return Attestation{}, err
	}
	// Modo 'log': registra a intenção de ancorar; o atestado permanece 'none' — nada de
	// 'anchored' sem transação real.
	if anchorMode == chain.AnchorModeLog && anchorer != nil {
		_, _ = anchorer.SendAnchor(ctx, digest)
	}
	// Índice público (verificação sem tenant no path).
	if _, err := tx.Exec(ctx, `
		INSERT INTO public.attestation_index (attestation_id, producer_id, event_id)
		VALUES ($1,$2,$3) ON CONFLICT (attestation_id) DO NOTHING`,
		a.ID, producerID, eventID); err != nil {
		return Attestation{}, err
	}
	return a, nil
}

// Current devolve o atestado vigente do evento (nil se ainda não fechado).
func Current(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (*Attestation, error) {
	return loadCurrent(ctx, tx, eventID)
}

func loadCurrent(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (*Attestation, error) {
	var a Attestation
	err := tx.QueryRow(ctx, `
		SELECT id, event_id, version, supersedes_id, payload, digest, signature, closed_at, anchor_status, anchor_tx_hash, anchored_at
		  FROM event_attestations WHERE event_id=$1 AND supersedes_id IS NULL`, eventID).
		Scan(&a.ID, &a.EventID, &a.Version, &a.SupersedesID, &a.Payload, &a.Digest, &a.Signature, &a.ClosedAt, &a.AnchorStatus, &a.AnchorTxHash, &a.AnchoredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// Get devolve um atestado por id (público, sem tenant no path).
func Get(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*Attestation, error) {
	var a Attestation
	err := tx.QueryRow(ctx, `
		SELECT id, event_id, version, supersedes_id, payload, digest, signature, closed_at, anchor_status, anchor_tx_hash, anchored_at
		  FROM event_attestations WHERE id=$1`, id).
		Scan(&a.ID, &a.EventID, &a.Version, &a.SupersedesID, &a.Payload, &a.Digest, &a.Signature, &a.ClosedAt, &a.AnchorStatus, &a.AnchorTxHash, &a.AnchoredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// SupersededBy devolve o id da versão vigente que substituiu este atestado (nil se ainda
// é vigente).
func SupersededBy(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*uuid.UUID, error) {
	var newer *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM event_attestations WHERE supersedes_id=$1`, id).Scan(&newer)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return newer, nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
