// Package catalog é o domínio de catálogo do produtor (Etapa 1.2): eventos, lotes e
// cupons. Todas as funções operam sobre o schema do produtor e recebem uma pgx.Tx já
// escopada por tenancy.WithTenant (search_path = tenant_<id>, public). SQL à mão, sem
// sqlc.
//
// A peça que exige teste aqui é a VIRADA AUTOMÁTICA de lote: ao esgotar o estoque, o
// lote atual vira sold_out e o próximo (por posição, elegível por data) passa a active.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Categorias válidas do catálogo (espelha o CHECK da migration de events).
var categories = []string{"shows", "teatro", "festas", "esportes", "congressos", "cursos", "workshops", "gastronomia"}

// ValidCategory diz se c é uma categoria conhecida.
func ValidCategory(c string) bool {
	return slices.Contains(categories, c)
}

var (
	// ErrLotNotSellable: lote não está 'active' (ou não existe no schema).
	ErrLotNotSellable = errors.New("catalog: lote não está à venda")
	// ErrInsufficientStock: a quantidade pedida excede o estoque restante.
	ErrInsufficientStock = errors.New("catalog: estoque insuficiente no lote")
)

// Event é um evento do produtor.
type Event struct {
	ID                 uuid.UUID  `json:"id"`
	Title              string     `json:"title"`
	Description        *string    `json:"description,omitempty"`
	Category           string     `json:"category"`
	CoverURL           *string    `json:"cover_url,omitempty"`
	StartsAt           *time.Time `json:"starts_at,omitempty"`
	EndsAt             *time.Time `json:"ends_at,omitempty"`
	Address            *string    `json:"address,omitempty"`
	Lat                *float64   `json:"lat,omitempty"`
	Lng                *float64   `json:"lng,omitempty"`
	Capacity           *int       `json:"capacity,omitempty"`
	AgeRating          *string    `json:"age_rating,omitempty"`
	CancellationPolicy *string    `json:"cancellation_policy,omitempty"`
	Terms              *string    `json:"terms,omitempty"`
	HasSeatMap         bool       `json:"has_seat_map"`
	Status             string     `json:"status"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

const eventCols = `id, title, description, category, cover_url, starts_at, ends_at,
	address, lat, lng, capacity, age_rating, cancellation_policy, terms, has_seat_map,
	status, created_at, updated_at`

func scanEvent(row pgx.Row) (Event, error) {
	var e Event
	err := row.Scan(&e.ID, &e.Title, &e.Description, &e.Category, &e.CoverURL, &e.StartsAt,
		&e.EndsAt, &e.Address, &e.Lat, &e.Lng, &e.Capacity, &e.AgeRating,
		&e.CancellationPolicy, &e.Terms, &e.HasSeatMap, &e.Status, &e.CreatedAt, &e.UpdatedAt)
	return e, err
}

// CreateEvent insere um evento (nasce 'draft'). Valida a categoria no chamador.
func CreateEvent(ctx context.Context, tx pgx.Tx, e Event) (Event, error) {
	row := tx.QueryRow(ctx, `
		INSERT INTO events (title, description, category, cover_url, starts_at, ends_at,
			address, lat, lng, capacity, age_rating, cancellation_policy, terms, has_seat_map)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING `+eventCols,
		e.Title, e.Description, e.Category, e.CoverURL, e.StartsAt, e.EndsAt, e.Address,
		e.Lat, e.Lng, e.Capacity, e.AgeRating, e.CancellationPolicy, e.Terms, e.HasSeatMap)
	out, err := scanEvent(row)
	if err != nil {
		return Event{}, fmt.Errorf("criar evento: %w", err)
	}
	return out, nil
}

// GetEvent devolve um evento por id.
func GetEvent(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Event, error) {
	return scanEvent(tx.QueryRow(ctx, `SELECT `+eventCols+` FROM events WHERE id = $1`, id))
}

// ListEvents lista os eventos do produtor (mais recentes primeiro).
func ListEvents(ctx context.Context, tx pgx.Tx) ([]Event, error) {
	rows, err := tx.Query(ctx, `SELECT `+eventCols+` FROM events ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Lot é um lote de um evento.
type Lot struct {
	ID         uuid.UUID  `json:"id"`
	EventID    uuid.UUID  `json:"event_id"`
	Name       string     `json:"name"`
	PriceCents int64      `json:"price_cents"`
	Stock      int        `json:"stock"`
	Sold       int        `json:"sold"`
	StartsAt   *time.Time `json:"starts_at,omitempty"`
	EndsAt     *time.Time `json:"ends_at,omitempty"`
	Position   int        `json:"position"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

const lotCols = `id, event_id, name, price_cents, stock, sold, starts_at, ends_at,
	position, status, created_at, updated_at`

func scanLot(row pgx.Row) (Lot, error) {
	var l Lot
	err := row.Scan(&l.ID, &l.EventID, &l.Name, &l.PriceCents, &l.Stock, &l.Sold,
		&l.StartsAt, &l.EndsAt, &l.Position, &l.Status, &l.CreatedAt, &l.UpdatedAt)
	return l, err
}

// CreateLot insere um lote (nasce 'scheduled'). A posição ordena a fila de virada.
func CreateLot(ctx context.Context, tx pgx.Tx, l Lot) (Lot, error) {
	row := tx.QueryRow(ctx, `
		INSERT INTO lots (event_id, name, price_cents, stock, starts_at, ends_at, position)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING `+lotCols,
		l.EventID, l.Name, l.PriceCents, l.Stock, l.StartsAt, l.EndsAt, l.Position)
	out, err := scanLot(row)
	if err != nil {
		return Lot{}, fmt.Errorf("criar lote: %w", err)
	}
	return out, nil
}

// ListLots lista os lotes de um evento por posição.
func ListLots(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) ([]Lot, error) {
	rows, err := tx.Query(ctx, `SELECT `+lotCols+` FROM lots WHERE event_id = $1 ORDER BY position, created_at`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Lot
	for rows.Next() {
		l, err := scanLot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetLot devolve um lote por id.
func GetLot(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Lot, error) {
	return scanLot(tx.QueryRow(ctx, `SELECT `+lotCols+` FROM lots WHERE id = $1`, id))
}

// PublishEvent publica o evento, registra-o no diretório público (para o comprador
// sem conta resolver evento→produtor) e ativa o lote inicial (o primeiro elegível por
// posição/data), se ainda não houver lote ativo. Idempotente.
func PublishEvent(ctx context.Context, tx pgx.Tx, producerID, eventID uuid.UUID) error {
	var title, category string
	var startsAt *time.Time
	err := tx.QueryRow(ctx, `UPDATE events SET status='published', updated_at=now()
		WHERE id=$1 AND status IN ('draft','published')
		RETURNING title, category, starts_at`, eventID).Scan(&title, &category, &startsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("evento não encontrado ou em estado não publicável")
	}
	if err != nil {
		return fmt.Errorf("publicar evento: %w", err)
	}
	// Diretório público (public está no search_path do WithTenant).
	if _, err := tx.Exec(ctx, `
		INSERT INTO public.event_directory (event_id, producer_id, title, category, starts_at)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (event_id) DO UPDATE
		   SET producer_id = EXCLUDED.producer_id, title = EXCLUDED.title,
		       category = EXCLUDED.category, starts_at = EXCLUDED.starts_at`,
		eventID, producerID, title, category, startsAt); err != nil {
		return fmt.Errorf("registrar no diretório público: %w", err)
	}
	return activateNextLot(ctx, tx, eventID, -1)
}

// SellFromLot debita `qty` do estoque do lote ATIVO, de forma atômica, e dispara a
// virada quando o estoque esgota. É o gancho que o checkout (Etapa 1.4) vai chamar.
// A condição `sold + qty <= stock` no próprio UPDATE impede vender além do estoque
// mesmo sob concorrência.
func SellFromLot(ctx context.Context, tx pgx.Tx, lotID uuid.UUID, qty int) (Lot, error) {
	if qty <= 0 {
		return Lot{}, fmt.Errorf("catalog: qty deve ser > 0")
	}
	row := tx.QueryRow(ctx, `
		UPDATE lots SET sold = sold + $2, updated_at = now()
		 WHERE id = $1 AND status = 'active' AND sold + $2 <= stock
		 RETURNING `+lotCols, lotID, qty)
	lot, err := scanLot(row)
	if errors.Is(err, pgx.ErrNoRows) {
		// Distingue "não vendável" de "sem estoque" para o chamador reagir.
		cur, gErr := GetLot(ctx, tx, lotID)
		if gErr != nil {
			return Lot{}, ErrLotNotSellable
		}
		if cur.Status != "active" {
			return Lot{}, ErrLotNotSellable
		}
		return Lot{}, ErrInsufficientStock
	}
	if err != nil {
		return Lot{}, fmt.Errorf("vender do lote: %w", err)
	}

	// Virada: esgotou → sold_out e ativa o próximo elegível por posição.
	if lot.Sold >= lot.Stock {
		if _, err := tx.Exec(ctx, `UPDATE lots SET status='sold_out', updated_at=now() WHERE id=$1`, lot.ID); err != nil {
			return Lot{}, fmt.Errorf("marcar sold_out: %w", err)
		}
		lot.Status = "sold_out"
		if err := activateNextLot(ctx, tx, lot.EventID, lot.Position); err != nil {
			return Lot{}, err
		}
	}
	return lot, nil
}

// ReconcileLots aplica a virada por DATA: fecha lotes vencidos e, se não houver lote
// ativo, ativa o próximo elegível. Idempotente; pensado para uma varredura periódica.
func ReconcileLots(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `
		UPDATE lots SET status='closed', updated_at=now()
		 WHERE event_id=$1 AND status IN ('scheduled','active')
		   AND ends_at IS NOT NULL AND ends_at <= now()`, eventID); err != nil {
		return fmt.Errorf("fechar lotes vencidos: %w", err)
	}
	return activateNextLot(ctx, tx, eventID, -1)
}

// activateNextLot ativa o primeiro lote 'scheduled' com posição > afterPos, elegível
// por data, SE não existir lote ativo no evento. Use afterPos=-1 para "qualquer".
// FOR UPDATE SKIP LOCKED evita corrida entre duas viradas simultâneas.
func activateNextLot(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, afterPos int) error {
	_, err := tx.Exec(ctx, `
		UPDATE lots SET status='active', updated_at=now()
		 WHERE id = (
			SELECT id FROM lots
			 WHERE event_id=$1 AND status='scheduled' AND position > $2
			   AND (starts_at IS NULL OR starts_at <= now())
			   AND (ends_at IS NULL OR ends_at > now())
			 ORDER BY position
			 LIMIT 1
			 FOR UPDATE SKIP LOCKED
		 )
		 AND NOT EXISTS (SELECT 1 FROM lots WHERE event_id=$1 AND status='active')`,
		eventID, afterPos)
	if err != nil {
		return fmt.Errorf("ativar próximo lote: %w", err)
	}
	return nil
}

// Coupon é um cupom de desconto.
type Coupon struct {
	ID            uuid.UUID  `json:"id"`
	EventID       *uuid.UUID `json:"event_id,omitempty"`
	Code          string     `json:"code"`
	DiscountPct   *float64   `json:"discount_pct,omitempty"`
	DiscountCents *int64     `json:"discount_cents,omitempty"`
	MaxUses       *int       `json:"max_uses,omitempty"`
	Uses          int        `json:"uses"`
	ValidFrom     *time.Time `json:"valid_from,omitempty"`
	ValidUntil    *time.Time `json:"valid_until,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

const couponCols = `id, event_id, code, discount_pct, discount_cents, max_uses, uses,
	valid_from, valid_until, created_at`

func scanCoupon(row pgx.Row) (Coupon, error) {
	var c Coupon
	err := row.Scan(&c.ID, &c.EventID, &c.Code, &c.DiscountPct, &c.DiscountCents,
		&c.MaxUses, &c.Uses, &c.ValidFrom, &c.ValidUntil, &c.CreatedAt)
	return c, err
}

// CreateCoupon insere um cupom (limite de uso e validade opcionais).
func CreateCoupon(ctx context.Context, tx pgx.Tx, c Coupon) (Coupon, error) {
	row := tx.QueryRow(ctx, `
		INSERT INTO coupons (event_id, code, discount_pct, discount_cents, max_uses, valid_from, valid_until)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING `+couponCols,
		c.EventID, c.Code, c.DiscountPct, c.DiscountCents, c.MaxUses, c.ValidFrom, c.ValidUntil)
	out, err := scanCoupon(row)
	if err != nil {
		return Coupon{}, fmt.Errorf("criar cupom: %w", err)
	}
	return out, nil
}

// ListCoupons lista os cupons de um evento.
func ListCoupons(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) ([]Coupon, error) {
	rows, err := tx.Query(ctx, `SELECT `+couponCols+` FROM coupons WHERE event_id = $1 ORDER BY created_at`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Coupon
	for rows.Next() {
		c, err := scanCoupon(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
