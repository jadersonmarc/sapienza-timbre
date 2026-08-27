// Package catalog é o domínio de catálogo do produtor (Etapa 1.2): eventos, lotes,
// categorias e cupons. Todas as funções operam sobre o schema do produtor e recebem uma
// pgx.Tx já escopada por tenancy.WithTenant (search_path = tenant_<id>, public). SQL à
// mão, sem sqlc.
//
// O lote NÃO tem coluna de status: a vigência é DERIVADA (janela por data + capacidade
// pelos contadores quantity/sold_count/held_count, com o CHECK lots_capacity_chk como
// backstop do schema). A "virada" é só resolver o lote corrente (CurrentLot) — uma única
// consulta cobre virada por data E por esgotamento, sem escrever estado.
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

var (
	// ErrInsufficientStock: a quantidade pedida excede a capacidade restante do lote.
	ErrInsufficientStock = errors.New("catalog: capacidade insuficiente no lote")
	// ErrNoCurrentLot: nenhum lote vigente (por data/capacidade) no evento.
	ErrNoCurrentLot = errors.New("catalog: nenhum lote vigente no evento")
	// ErrCategoryInvalid: categoria (slug) inexistente ou inativa.
	ErrCategoryInvalid = errors.New("catalog: categoria inválida")
	// ErrInvalidTransition: transição de estado do evento não permitida.
	ErrInvalidTransition = errors.New("catalog: transição de estado inválida")
	// ErrNotPublishable: publicação sem lote, setor, categoria ou data futura.
	ErrNotPublishable = errors.New("catalog: evento não satisfaz as regras de publicação")
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
	City               *string    `json:"city,omitempty"`
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
	address, city, lat, lng, capacity, age_rating, cancellation_policy, terms, has_seat_map,
	status, created_at, updated_at`

func scanEvent(row pgx.Row) (Event, error) {
	var e Event
	err := row.Scan(&e.ID, &e.Title, &e.Description, &e.Category, &e.CoverURL, &e.StartsAt,
		&e.EndsAt, &e.Address, &e.City, &e.Lat, &e.Lng, &e.Capacity, &e.AgeRating,
		&e.CancellationPolicy, &e.Terms, &e.HasSeatMap, &e.Status, &e.CreatedAt, &e.UpdatedAt)
	return e, err
}

// CreateEvent insere um evento (nasce 'draft'). A categoria é resolvida contra
// event_categories (ativa) — a escrita de nascimento de category/category_id acontece
// aqui, de forma atômica; toda MUDANÇA posterior passa por applyCategory (escritor
// único do campo vivo).
func CreateEvent(ctx context.Context, tx pgx.Tx, e Event) (Event, error) {
	catID, slug, err := resolveCategory(ctx, tx, e.Category)
	if err != nil {
		return Event{}, err
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO events (title, description, category, category_id, cover_url, starts_at, ends_at,
			address, city, lat, lng, capacity, age_rating, cancellation_policy, terms, has_seat_map)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		RETURNING `+eventCols,
		e.Title, e.Description, slug, catID, e.CoverURL, e.StartsAt, e.EndsAt, e.Address,
		e.City, e.Lat, e.Lng, e.Capacity, e.AgeRating, e.CancellationPolicy, e.Terms, e.HasSeatMap)
	out, err := scanEvent(row)
	if err != nil {
		return Event{}, fmt.Errorf("criar evento: %w", err)
	}
	return out, nil
}

// resolveCategory valida o slug contra event_categories (existente e ativa) e devolve o
// id + o slug canônico. Ponto ÚNICO de resolução de categoria (usado por CreateEvent no
// nascimento e por applyCategory nas mudanças).
func resolveCategory(ctx context.Context, tx pgx.Tx, slug string) (uuid.UUID, string, error) {
	var id uuid.UUID
	var canonical string
	err := tx.QueryRow(ctx, `SELECT id, slug FROM event_categories WHERE slug=$1 AND active`, slug).Scan(&id, &canonical)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", ErrCategoryInvalid
	}
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("resolver categoria: %w", err)
	}
	return id, canonical, nil
}

// applyCategory é o ESCRITOR ÚNICO da categoria de um evento já existente: resolve o
// slug, grava category_id E category (denormalizado) e sincroniza o event_directory
// público, tudo na mesma tx. Nenhuma outra função pode fazer `UPDATE events SET
// category(_id)`. Idempotente. Sincroniza o diretório só se o evento já estiver lá
// (publicado); caso contrário, PublishEvent o cria depois com a categoria coerente.
func applyCategory(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, slug string) error {
	catID, canonical, err := resolveCategory(ctx, tx, slug)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE events SET category=$2, category_id=$3, updated_at=now() WHERE id=$1`,
		eventID, canonical, catID)
	if err != nil {
		return fmt.Errorf("aplicar categoria: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("evento não encontrado")
	}
	// Sincroniza o diretório público apenas se o evento já estiver registrado.
	if _, err := tx.Exec(ctx, `
		UPDATE public.event_directory SET category=$2 WHERE event_id=$1`, eventID, canonical); err != nil {
		return fmt.Errorf("sincronizar categoria no diretório: %w", err)
	}
	return nil
}

// ListCategories lista as categorias ativas em ordem de exibição.
func ListCategories(ctx context.Context, tx pgx.Tx) ([]Category, error) {
	rows, err := tx.Query(ctx, `SELECT id, slug, name, sort_order, active
		FROM event_categories WHERE active ORDER BY sort_order, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Slug, &c.Name, &c.SortOrder, &c.Active); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Category é uma categoria do catálogo.
type Category struct {
	ID        uuid.UUID `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	SortOrder int       `json:"sort_order"`
	Active    bool      `json:"active"`
}

// PatchEvent aplica alterações parciais a um evento em 'draft'/'pending_review'. Só os
// campos não-nil são tocados. A categoria (se informada) passa pelo escritor único
// applyCategory. Evento 'published' não pode trocar de categoria (regra de serviço).
func PatchEvent(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, p EventPatch) (Event, error) {
	cur, err := GetEvent(ctx, tx, eventID)
	if err != nil {
		return Event{}, err
	}
	if p.Category != nil {
		if cur.Status == "published" && *p.Category != cur.Category {
			return Event{}, fmt.Errorf("%w: evento publicado não troca de categoria", ErrInvalidTransition)
		}
		if err := applyCategory(ctx, tx, eventID, *p.Category); err != nil {
			return Event{}, err
		}
	}
	// Demais campos simples (nunca category/category_id — esses só via applyCategory).
	if _, err := tx.Exec(ctx, `
		UPDATE events SET
			title = COALESCE($2, title),
			description = COALESCE($3, description),
			cover_url = COALESCE($4, cover_url),
			starts_at = COALESCE($5, starts_at),
			ends_at = COALESCE($6, ends_at),
			address = COALESCE($7, address),
			city = COALESCE($8, city),
			capacity = COALESCE($9, capacity),
			age_rating = COALESCE($10, age_rating),
			cancellation_policy = COALESCE($11, cancellation_policy),
			terms = COALESCE($12, terms),
			updated_at = now()
		WHERE id = $1`,
		eventID, p.Title, p.Description, p.CoverURL, p.StartsAt, p.EndsAt, p.Address,
		p.City, p.Capacity, p.AgeRating, p.CancellationPolicy, p.Terms); err != nil {
		return Event{}, fmt.Errorf("atualizar evento: %w", err)
	}
	return GetEvent(ctx, tx, eventID)
}

// EventPatch são os campos alteráveis de um evento (nil = não muda).
type EventPatch struct {
	Title              *string
	Description        *string
	Category           *string
	CoverURL           *string
	StartsAt           *time.Time
	EndsAt             *time.Time
	Address            *string
	City               *string
	Capacity           *int
	AgeRating          *string
	CancellationPolicy *string
	Terms              *string
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

// Lot é um lote de um evento. Sem status: a vigência é derivada (janela por data +
// capacidade pelos contadores). sort_order ordena a fila de virada (único por evento).
type Lot struct {
	ID         uuid.UUID  `json:"id"`
	EventID    uuid.UUID  `json:"event_id"`
	Name       string     `json:"name"`
	PriceCents int64      `json:"price_cents"`
	Quantity   int        `json:"quantity"`
	SoldCount  int        `json:"sold_count"`
	HeldCount  int        `json:"held_count"`
	StartsAt   *time.Time `json:"starts_at,omitempty"`
	EndsAt     *time.Time `json:"ends_at,omitempty"`
	SortOrder  int        `json:"sort_order"`
	// Faixa de quantidade por compra. Um "ingresso duplo" é um lote com mínimo 2 e máximo
	// 2, e o preço acima continua sendo o UNITÁRIO — o comprador paga preço × quantidade.
	// A compra gera N ingressos independentes; combo é regra de COMPRA, não vínculo entre
	// ingressos.
	MinPurchaseQuantity int       `json:"min_purchase_quantity"`
	MaxPurchaseQuantity *int      `json:"max_purchase_quantity,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// ErrPurchaseRange é a quantidade fora da faixa do lote.
var ErrPurchaseRange = errors.New("catalog: quantidade fora da faixa deste ingresso")

// ErrBadPurchaseRange é a faixa mal declarada pelo produtor.
var ErrBadPurchaseRange = errors.New("catalog: o máximo por compra não pode ser menor que o mínimo")

// CheckPurchaseQuantity valida a quantidade contra a faixa do lote. Devolve ErrPurchaseRange
// com a faixa, para a mensagem dizer o que fazer em vez de só recusar.
func (l Lot) CheckPurchaseQuantity(qty int) error {
	if qty < l.MinPurchaseQuantity {
		return fmt.Errorf("%w: mínimo de %d por compra", ErrPurchaseRange, l.MinPurchaseQuantity)
	}
	if l.MaxPurchaseQuantity != nil && qty > *l.MaxPurchaseQuantity {
		return fmt.Errorf("%w: máximo de %d por compra", ErrPurchaseRange, *l.MaxPurchaseQuantity)
	}
	return nil
}

const lotCols = `id, event_id, name, price_cents, quantity, sold_count, held_count,
	starts_at, ends_at, sort_order, min_purchase_quantity, max_purchase_quantity,
	created_at, updated_at`

func scanLot(row pgx.Row) (Lot, error) {
	var l Lot
	err := row.Scan(&l.ID, &l.EventID, &l.Name, &l.PriceCents, &l.Quantity, &l.SoldCount,
		&l.HeldCount, &l.StartsAt, &l.EndsAt, &l.SortOrder,
		&l.MinPurchaseQuantity, &l.MaxPurchaseQuantity, &l.CreatedAt, &l.UpdatedAt)
	return l, err
}

// CreateLot insere um lote. sort_order ordena a fila de virada.
func CreateLot(ctx context.Context, tx pgx.Tx, l Lot) (Lot, error) {
	if l.MinPurchaseQuantity <= 0 {
		l.MinPurchaseQuantity = 1
	}
	if l.MaxPurchaseQuantity != nil && *l.MaxPurchaseQuantity < l.MinPurchaseQuantity {
		return Lot{}, ErrBadPurchaseRange
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO lots (event_id, name, price_cents, quantity, starts_at, ends_at, sort_order,
		                  min_purchase_quantity, max_purchase_quantity)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING `+lotCols,
		l.EventID, l.Name, l.PriceCents, l.Quantity, l.StartsAt, l.EndsAt, l.SortOrder,
		l.MinPurchaseQuantity, l.MaxPurchaseQuantity)
	out, err := scanLot(row)
	if err != nil {
		return Lot{}, fmt.Errorf("criar lote: %w", err)
	}
	return out, nil
}

// ListLots lista os lotes de um evento por sort_order.
func ListLots(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) ([]Lot, error) {
	rows, err := tx.Query(ctx, `SELECT `+lotCols+` FROM lots WHERE event_id = $1 ORDER BY sort_order, created_at`, eventID)
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

// CurrentLot resolve o lote VIGENTE do evento: o primeiro (por sort_order) dentro da
// janela de datas e com capacidade restante (sold_count+held_count < quantity). Uma
// única consulta cobre a virada por DATA e por ESGOTAMENTO — não há estado a escrever.
// ErrNoCurrentLot se nenhum for elegível.
func CurrentLot(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (Lot, error) {
	row := tx.QueryRow(ctx, `SELECT `+lotCols+` FROM lots
		 WHERE event_id = $1
		   AND (starts_at IS NULL OR starts_at <= now())
		   AND (ends_at IS NULL OR ends_at > now())
		   -- Saldo menor que o mínimo por compra é lote ESGOTADO: sobrar 1 lugar num
		   -- ingresso duplo significa que aquele lote acabou, e continuar oferecendo
		   -- levaria o comprador a uma recusa no fim do checkout.
		   AND quantity - sold_count - held_count >= min_purchase_quantity
		 ORDER BY sort_order, created_at
		 LIMIT 1`, eventID)
	lot, err := scanLot(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lot{}, ErrNoCurrentLot
	}
	if err != nil {
		return Lot{}, fmt.Errorf("resolver lote vigente: %w", err)
	}
	return lot, nil
}

// ReserveFromLot segura `qty` unidades no lote (held_count += qty), de forma atômica: o
// próprio UPDATE só afeta a linha se ainda couber (sold_count+held_count+qty <=
// quantity). Zero linhas afetadas → ErrInsufficientStock (o CHECK lots_capacity_chk é o
// backstop do schema, não o caminho de erro). A tx segue utilizável após a falha.
func ReserveFromLot(ctx context.Context, tx pgx.Tx, lotID uuid.UUID, qty int) error {
	if qty <= 0 {
		return fmt.Errorf("catalog: qty deve ser > 0")
	}
	tag, err := tx.Exec(ctx, `
		UPDATE lots SET held_count = held_count + $2, updated_at = now()
		 WHERE id = $1 AND sold_count + held_count + $2 <= quantity`, lotID, qty)
	if err != nil {
		return fmt.Errorf("reservar do lote: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrInsufficientStock
	}
	return nil
}

// ReleaseFromLot devolve `qty` unidades seguras (held_count -= qty), com piso em 0
// (Correção 4.2): release repetido nunca leva o contador a negativo.
func ReleaseFromLot(ctx context.Context, tx pgx.Tx, lotID uuid.UUID, qty int) error {
	if qty <= 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE lots SET held_count = GREATEST(held_count - $2, 0), updated_at = now()
		 WHERE id = $1`, lotID, qty); err != nil {
		return fmt.Errorf("liberar do lote: %w", err)
	}
	return nil
}

// ConfirmFromLot converte `qty` de segurado em vendido (held_count -= qty, sold_count +=
// qty), com piso no held. Devolve soldOut=true se o lote ficou cheio. É o passo do
// webhook (pagamento confirmado).
func ConfirmFromLot(ctx context.Context, tx pgx.Tx, lotID uuid.UUID, qty int) (bool, error) {
	if qty <= 0 {
		return false, fmt.Errorf("catalog: qty deve ser > 0")
	}
	var soldCount, heldCount, quantity int
	err := tx.QueryRow(ctx, `
		UPDATE lots SET sold_count = sold_count + $2,
		                held_count = GREATEST(held_count - $2, 0),
		                updated_at = now()
		 WHERE id = $1
		 RETURNING sold_count, held_count, quantity`, lotID, qty).Scan(&soldCount, &heldCount, &quantity)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("lote não encontrado")
	}
	if err != nil {
		return false, fmt.Errorf("confirmar do lote: %w", err)
	}
	return soldCount+heldCount >= quantity, nil
}

// RefundFromLot devolve capacidade ao lote no estorno (sold_count -= qty), com piso em 0
// (Correção 4.2): estorno duplicado nunca leva o contador a negativo.
func RefundFromLot(ctx context.Context, tx pgx.Tx, lotID uuid.UUID, qty int) error {
	if qty <= 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE lots SET sold_count = GREATEST(sold_count - $2, 0), updated_at = now()
		 WHERE id = $1`, lotID, qty); err != nil {
		return fmt.Errorf("estornar ao lote: %w", err)
	}
	return nil
}

// validTransitions define o ciclo de vida do evento. A publicação é a transição
// pending_review→published (ou draft→published, para o produtor sem etapa de revisão);
// suspender/cancelar/finalizar partem de published.
var validTransitions = map[string][]string{
	"draft":          {"pending_review", "published", "cancelled"},
	"pending_review": {"published", "draft", "cancelled"},
	"published":      {"finished", "suspended", "cancelled"},
	"suspended":      {"published", "cancelled"},
}

// canTransition diz se from→to é permitida.
func canTransition(from, to string) bool {
	return slices.Contains(validTransitions[from], to)
}

// TransitionEvent muda o estado do evento validando o ciclo de vida. Erro claro
// (ErrInvalidTransition) na transição não permitida.
func TransitionEvent(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, to string) error {
	var from string
	if err := tx.QueryRow(ctx, `SELECT status FROM events WHERE id=$1 FOR UPDATE`, eventID).Scan(&from); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("evento não encontrado")
		}
		return err
	}
	if from == to {
		return nil // idempotente
	}
	if !canTransition(from, to) {
		return fmt.Errorf("%w: %s→%s", ErrInvalidTransition, from, to)
	}
	if _, err := tx.Exec(ctx, `UPDATE events SET status=$2, updated_at=now() WHERE id=$1`, eventID, to); err != nil {
		return fmt.Errorf("transicionar evento: %w", err)
	}
	// Evento que sai do ar (suspenso/cancelado/finalizado) não pode ficar listado no
	// diretório público. Removê-lo daqui é o pareado do registro no publish.
	if to == "suspended" || to == "cancelled" || to == "finished" {
		if _, err := tx.Exec(ctx, `DELETE FROM public.event_directory WHERE event_id=$1`, eventID); err != nil {
			return fmt.Errorf("remover do diretório público: %w", err)
		}
	}
	return nil
}

// PublishEvent valida as regras de publicação (≥1 lote, ≥1 setor, categoria e data de
// início futura), transiciona o evento para 'published' e o registra no diretório
// público (para o comprador sem conta resolver evento→produtor). Idempotente.
func PublishEvent(ctx context.Context, tx pgx.Tx, producerID, eventID uuid.UUID) error {
	var status, title, category string
	var startsAt *time.Time
	var coverURL, city *string
	var lat, lng *float64
	err := tx.QueryRow(ctx, `SELECT status, title, category, starts_at, cover_url, city, lat, lng
		FROM events WHERE id=$1 FOR UPDATE`, eventID).
		Scan(&status, &title, &category, &startsAt, &coverURL, &city, &lat, &lng)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("evento não encontrado")
	}
	if err != nil {
		return fmt.Errorf("publicar evento: %w", err)
	}
	// Validações de publicação (mesmo quando já publicado, mantém o diretório coerente).
	if err := checkPublishable(ctx, tx, eventID, startsAt); err != nil {
		return err
	}
	if status != "published" {
		if !canTransition(status, "published") {
			return fmt.Errorf("%w: %s→published", ErrInvalidTransition, status)
		}
		if _, err := tx.Exec(ctx, `UPDATE events SET status='published', updated_at=now() WHERE id=$1`, eventID); err != nil {
			return fmt.Errorf("publicar evento: %w", err)
		}
	}
	minPrice, err := eventMinPrice(ctx, tx, eventID)
	if err != nil {
		return err
	}
	// Diretório público enriquecido (card do diretório + SEO). public está no search_path.
	if _, err := tx.Exec(ctx, `
		INSERT INTO public.event_directory
		    (event_id, producer_id, title, category, starts_at, cover_url, city, lat, lng, min_price_cents, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, now())
		ON CONFLICT (event_id) DO UPDATE
		   SET producer_id = EXCLUDED.producer_id, title = EXCLUDED.title,
		       category = EXCLUDED.category, starts_at = EXCLUDED.starts_at,
		       cover_url = EXCLUDED.cover_url, city = EXCLUDED.city,
		       lat = EXCLUDED.lat, lng = EXCLUDED.lng,
		       min_price_cents = EXCLUDED.min_price_cents, updated_at = now()`,
		eventID, producerID, title, category, startsAt, coverURL, city, lat, lng, minPrice); err != nil {
		return fmt.Errorf("registrar no diretório público: %w", err)
	}
	return nil
}

// eventMinPrice devolve o menor preço vendável do evento — o menor entre os preços de
// lote (pista) e as regras de preço por setor (assento). Considera só lotes com capacidade
// restante (o menor preço REAL muda quando o lote esgota). nil se não houver preço.
func eventMinPrice(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (*int64, error) {
	var min *int64
	err := tx.QueryRow(ctx, `
		SELECT LEAST(
		  (SELECT min(price_cents) FROM lots
		     WHERE event_id = $1 AND sold_count + held_count < quantity),
		  (SELECT min(spr.price_cents) FROM sector_price_rules spr
		     JOIN lots l ON l.id = spr.lot_id
		    WHERE l.event_id = $1 AND l.sold_count + l.held_count < l.quantity)
		)`, eventID).Scan(&min)
	if err != nil {
		return nil, fmt.Errorf("calcular preço mínimo: %w", err)
	}
	return min, nil
}

// ResyncMinPrice recalcula o min_price do evento no diretório público. Chamado na virada
// de lote (o menor preço real muda quando um lote esgota — §3.10). No-op se o evento não
// estiver no diretório (não publicado).
func ResyncMinPrice(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) error {
	min, err := eventMinPrice(ctx, tx, eventID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE public.event_directory SET min_price_cents = $2, updated_at = now()
		 WHERE event_id = $1`, eventID, min); err != nil {
		return fmt.Errorf("resincronizar preço mínimo: %w", err)
	}
	return nil
}

// checkPublishable garante as regras de publicação: ≥1 lote, categoria (sempre presente
// pela FK), data de início futura e — para evento COM mapa (has_seat_map) — ≥1 setor. Um
// evento de pista (sem mapa) vende pelo lote, sem setor; exigir setor dele seria defeito.
// ErrNotPublishable se algo falta.
func checkPublishable(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, startsAt *time.Time) error {
	if startsAt == nil || !startsAt.After(time.Now()) {
		return fmt.Errorf("%w: data de início ausente ou no passado", ErrNotPublishable)
	}
	var lots, sectors int
	var hasSeatMap bool
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM lots WHERE event_id=$1),
		(SELECT count(*) FROM sectors WHERE event_id=$1),
		(SELECT has_seat_map FROM events WHERE id=$1)`, eventID).Scan(&lots, &sectors, &hasSeatMap); err != nil {
		return fmt.Errorf("verificar publicabilidade: %w", err)
	}
	if lots == 0 {
		return fmt.Errorf("%w: exige ao menos um lote", ErrNotPublishable)
	}
	if hasSeatMap && sectors == 0 {
		return fmt.Errorf("%w: evento com mapa exige ao menos um setor", ErrNotPublishable)
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
