package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrStandingHasNoSeats: não se gera assento em setor 'standing' (pista).
var ErrStandingHasNoSeats = errors.New("catalog: setor standing não tem assentos")

// Sector é um setor do evento (editor por grade, não canvas livre).
type Sector struct {
	ID          uuid.UUID       `json:"id"`
	EventID     uuid.UUID       `json:"event_id"`
	Name        string          `json:"name"`
	Kind        string          `json:"kind"` // standing | seated | table
	SellUnit    *string         `json:"sell_unit,omitempty"`
	Rows        *int            `json:"rows,omitempty"`
	SeatsPerRow *int            `json:"seats_per_row,omitempty"`
	NamingRule  json.RawMessage `json:"naming_rule,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// CreateSector insere um setor.
func CreateSector(ctx context.Context, tx pgx.Tx, s Sector) (Sector, error) {
	naming := s.NamingRule
	if len(naming) == 0 {
		naming = json.RawMessage("{}")
	}
	var out Sector
	err := tx.QueryRow(ctx, `
		INSERT INTO sectors (event_id, name, kind, sell_unit, rows, seats_per_row, naming_rule)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING id, event_id, name, kind, sell_unit, rows, seats_per_row, naming_rule, created_at`,
		s.EventID, s.Name, s.Kind, s.SellUnit, s.Rows, s.SeatsPerRow, naming,
	).Scan(&out.ID, &out.EventID, &out.Name, &out.Kind, &out.SellUnit, &out.Rows, &out.SeatsPerRow, &out.NamingRule, &out.CreatedAt)
	if err != nil {
		return Sector{}, fmt.Errorf("criar setor: %w", err)
	}
	return out, nil
}

// ListSectors lista os setores de um evento.
func ListSectors(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) ([]Sector, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, event_id, name, kind, sell_unit, rows, seats_per_row, naming_rule, created_at
		  FROM sectors WHERE event_id = $1 ORDER BY created_at`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sector
	for rows.Next() {
		var s Sector
		if err := rows.Scan(&s.ID, &s.EventID, &s.Name, &s.Kind, &s.SellUnit, &s.Rows, &s.SeatsPerRow, &s.NamingRule, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Seat é um assento.
type Seat struct {
	ID            uuid.UUID `json:"id"`
	SectorID      uuid.UUID `json:"sector_id"`
	RowLabel      *string   `json:"row_label,omitempty"`
	Number        *string   `json:"number,omitempty"`
	BlockedReason *string   `json:"blocked_reason,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// rowLabel gera o rótulo da fileira i (0-based). "alpha": A..Z, AA.. ; caso contrário
// numérico 1-based.
func rowLabel(i int, style string) string {
	if style != "alpha" {
		return strconv.Itoa(i + 1)
	}
	label := ""
	i++ // 1-based para o esquema tipo planilha (A, B, ... Z, AA, ...)
	for i > 0 {
		i--
		label = string(rune('A'+(i%26))) + label
		i /= 26
	}
	return label
}

// GenerateSeats cria a grade de assentos de um setor: `rows` fileiras × `seatsPerRow`
// assentos, com rótulo de fileira por `rowStyle` ("alpha"|"numeric") e número
// sequencial 1..N. Idempotente por (sector_id, row_label, number). Devolve o total
// criado. Corredores (colunas vazias) ficam para o editor de layout mais rico.
func GenerateSeats(ctx context.Context, tx pgx.Tx, sectorID uuid.UUID, rows, seatsPerRow int, rowStyle string) (int, error) {
	if rows <= 0 || seatsPerRow <= 0 {
		return 0, fmt.Errorf("catalog: rows e seats_per_row devem ser > 0")
	}
	// Setor de pista (standing) não tem assentos numerados.
	var kind string
	if err := tx.QueryRow(ctx, `SELECT kind FROM sectors WHERE id=$1`, sectorID).Scan(&kind); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("setor não encontrado")
		}
		return 0, err
	}
	if kind == "standing" {
		return 0, ErrStandingHasNoSeats
	}
	created := 0
	for r := range rows {
		label := rowLabel(r, rowStyle)
		for n := 1; n <= seatsPerRow; n++ {
			tag, err := tx.Exec(ctx, `
				INSERT INTO seats (sector_id, row_label, number)
				VALUES ($1,$2,$3)
				ON CONFLICT (sector_id, row_label, number) DO NOTHING`,
				sectorID, label, strconv.Itoa(n))
			if err != nil {
				return created, fmt.Errorf("gerar assento %s%d: %w", label, n, err)
			}
			created += int(tag.RowsAffected())
		}
	}
	return created, nil
}

// ListSeats lista os assentos de um setor, em ordem de fileira/número.
func ListSeats(ctx context.Context, tx pgx.Tx, sectorID uuid.UUID) ([]Seat, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, sector_id, row_label, number, blocked_reason, created_at
		  FROM seats WHERE sector_id = $1
		 ORDER BY row_label, NULLIF(regexp_replace(number, '\D', '', 'g'), '')::int NULLS LAST, number`, sectorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Seat
	for rows.Next() {
		var s Seat
		if err := rows.Scan(&s.ID, &s.SectorID, &s.RowLabel, &s.Number, &s.BlockedReason, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// BlockSeat bloqueia (reason != "") ou desbloqueia (reason == "") um assento.
func BlockSeat(ctx context.Context, tx pgx.Tx, seatID uuid.UUID, reason string) error {
	var r *string
	if reason != "" {
		r = &reason
	}
	tag, err := tx.Exec(ctx, `UPDATE seats SET blocked_reason = $2 WHERE id = $1`, seatID, r)
	if err != nil {
		return fmt.Errorf("bloquear assento: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("assento não encontrado")
	}
	return nil
}

// SectorPrice é o preço de um setor dentro de um lote.
type SectorPrice struct {
	ID             uuid.UUID `json:"id"`
	LotID          uuid.UUID `json:"lot_id"`
	SectorID       uuid.UUID `json:"sector_id"`
	PriceCents     int64     `json:"price_cents"`
	HalfPriceCents *int64    `json:"half_price_cents,omitempty"`
}

// SetSectorPrice define (upsert) o preço de um setor num lote (plateia e balcão com
// valores distintos no mesmo lote).
func SetSectorPrice(ctx context.Context, tx pgx.Tx, p SectorPrice) (SectorPrice, error) {
	var out SectorPrice
	err := tx.QueryRow(ctx, `
		INSERT INTO sector_price_rules (lot_id, sector_id, price_cents, half_price_cents)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (lot_id, sector_id) DO UPDATE
		   SET price_cents = EXCLUDED.price_cents, half_price_cents = EXCLUDED.half_price_cents
		RETURNING id, lot_id, sector_id, price_cents, half_price_cents`,
		p.LotID, p.SectorID, p.PriceCents, p.HalfPriceCents,
	).Scan(&out.ID, &out.LotID, &out.SectorID, &out.PriceCents, &out.HalfPriceCents)
	if err != nil {
		return SectorPrice{}, fmt.Errorf("definir preço de setor: %w", err)
	}
	return out, nil
}

// VenueTemplate é um modelo de mapa reaproveitável (ativo do PRODUTOR, não do evento):
// teatro italiano, auditório, casa com mesas.
type VenueTemplate struct {
	ID        uuid.UUID       `json:"id"`
	Name      string          `json:"name"`
	Layout    json.RawMessage `json:"layout,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// CreateVenueTemplate insere um modelo de mapa do produtor.
func CreateVenueTemplate(ctx context.Context, tx pgx.Tx, name string, layout json.RawMessage) (VenueTemplate, error) {
	if len(layout) == 0 {
		layout = json.RawMessage("{}")
	}
	var out VenueTemplate
	err := tx.QueryRow(ctx, `
		INSERT INTO venue_templates (name, layout) VALUES ($1,$2)
		RETURNING id, name, layout, created_at`, name, layout).
		Scan(&out.ID, &out.Name, &out.Layout, &out.CreatedAt)
	if err != nil {
		return VenueTemplate{}, fmt.Errorf("criar modelo de mapa: %w", err)
	}
	return out, nil
}

// ListVenueTemplates lista os modelos de mapa do produtor.
func ListVenueTemplates(ctx context.Context, tx pgx.Tx) ([]VenueTemplate, error) {
	rows, err := tx.Query(ctx, `SELECT id, name, layout, created_at FROM venue_templates ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VenueTemplate
	for rows.Next() {
		var v VenueTemplate
		if err := rows.Scan(&v.ID, &v.Name, &v.Layout, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListSectorPrices lista os preços por setor de um lote.
func ListSectorPrices(ctx context.Context, tx pgx.Tx, lotID uuid.UUID) ([]SectorPrice, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, lot_id, sector_id, price_cents, half_price_cents
		  FROM sector_price_rules WHERE lot_id = $1 ORDER BY sector_id`, lotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SectorPrice
	for rows.Next() {
		var p SectorPrice
		if err := rows.Scan(&p.ID, &p.LotID, &p.SectorID, &p.PriceCents, &p.HalfPriceCents); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
