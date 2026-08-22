package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-timbre/internal/catalog"
	"github.com/jadersonmarc/sapienza-timbre/internal/inventory"
	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
)

// TTLs e extensão de reserva — PROVISÓRIOS, isolados. A sessão anônima expira antes da
// vinculada; o bind estende a reserva UMA vez por TIMBRE_CHECKOUT_AUTH_GRACE.
const (
	SessionAnonTTL     = 10 * time.Minute
	SessionAuthedTTL   = 60 * time.Minute
	DefaultAuthGrace   = 10 * time.Minute
	MaxOpenSessionsPerIP = 5
)

var (
	// ErrSessionNotFound: sessão inexistente.
	ErrSessionNotFound = errors.New("checkout: sessão não encontrada")
	// ErrSessionNotOpen: a sessão não está mais editável.
	ErrSessionNotOpen = errors.New("checkout: sessão não está aberta")
	// ErrSessionBoundToOther: a sessão já foi vinculada a outro comprador.
	ErrSessionBoundToOther = errors.New("checkout: sessão vinculada a outro comprador")
	// ErrSessionExpired: a sessão expirou.
	ErrSessionExpired = errors.New("checkout: sessão expirada")
	// ErrTooManySessions: teto de sessões abertas por IP atingido.
	ErrTooManySessions = errors.New("checkout: muitas sessões abertas deste IP")
)

// SessionItems é a seleção guardada na sessão (jsonb).
type SessionItems struct {
	LotID        uuid.UUID   `json:"lot_id"`
	Quantity     int         `json:"quantity"`
	SeatIDs      []uuid.UUID `json:"seat_ids"`
	HalfPriceQty int         `json:"half_price_qty"`
	CouponCode   string      `json:"coupon_code"`
	CampaignID   *uuid.UUID  `json:"campaign_id,omitempty"`
}

// Session é uma sessão de checkout.
type Session struct {
	ID           uuid.UUID
	EventID      uuid.UUID
	AnonToken    string
	SubjectID    *uuid.UUID
	ClientIP     string
	Items        SessionItems
	HoldID       *uuid.UUID
	Status       string
	GraceApplied bool
	ExpiresAt    time.Time
}

// CreateSession cria a sessão, reserva (lote + assentos) e devolve a sessão. A seleção
// sobrevive ao desvio de autenticação; a reserva é exclusiva (hold/held_count).
func CreateSession(ctx context.Context, tx pgx.Tx, req Request, clientIP string) (Session, error) {
	lot, holdID, err := reserve(ctx, tx, req)
	if err != nil {
		return Session{}, err
	}
	items := SessionItems{
		LotID: lot.ID, Quantity: req.Quantity, SeatIDs: req.SeatIDs,
		HalfPriceQty: req.HalfPriceQty, CouponCode: req.CouponCode, CampaignID: req.CampaignID,
	}
	raw, _ := json.Marshal(items)
	var s Session
	s.EventID = req.EventID
	s.AnonToken = uuid.NewString()
	s.ClientIP = clientIP
	s.Items = items
	s.HoldID = holdID
	s.Status = "open"
	s.ExpiresAt = time.Now().Add(SessionAnonTTL)
	err = tx.QueryRow(ctx, `
		INSERT INTO checkout_sessions (event_id, anon_token, client_ip, items, hold_id, status, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		s.EventID, s.AnonToken, s.ClientIP, raw, s.HoldID, s.Status, s.ExpiresAt).Scan(&s.ID)
	return s, err
}

// GetSession devolve uma sessão pelo id.
func GetSession(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Session, error) {
	return loadSession(ctx, tx, sessionCols+` WHERE id=$1`, id)
}

// GetSessionByToken devolve uma sessão pelo anon_token (retomada antes do acesso).
func GetSessionByToken(ctx context.Context, tx pgx.Tx, token string) (Session, error) {
	return loadSession(ctx, tx, sessionCols+` WHERE anon_token=$1`, token)
}

// UpdateSession substitui a seleção: libera a reserva anterior e re-reserva (lote+assentos),
// zerando o TTL anônimo. Só aceita sessão 'open'.
func UpdateSession(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID, req Request) (Session, error) {
	var s Session
	if err := lockSession(ctx, tx, sessionID, &s); err != nil {
		return Session{}, err
	}
	if s.Status != "open" {
		return Session{}, ErrSessionNotOpen
	}
	if s.HoldID != nil {
		if err := inventory.Release(ctx, tx, *s.HoldID); err != nil {
			return Session{}, err
		}
	} else {
		if err := releaseLot(ctx, tx, s.Items.LotID, s.Items.Quantity); err != nil {
			return Session{}, err
		}
	}
	lot, holdID, err := reserve(ctx, tx, req)
	if err != nil {
		return Session{}, err
	}
	items := SessionItems{
		LotID: lot.ID, Quantity: req.Quantity, SeatIDs: req.SeatIDs,
		HalfPriceQty: req.HalfPriceQty, CouponCode: req.CouponCode, CampaignID: req.CampaignID,
	}
	raw, _ := json.Marshal(items)
	exp := time.Now().Add(SessionAnonTTL)
	if _, err := tx.Exec(ctx, `
		UPDATE checkout_sessions SET items=$2, hold_id=$3, expires_at=$4, updated_at=now() WHERE id=$1`,
		sessionID, raw, holdID, exp); err != nil {
		return Session{}, err
	}
	s.Items = items
	s.HoldID = holdID
	s.ExpiresAt = exp
	return s, nil
}

// BindSession vincula a sessão ao comprador (subject), marca 'authenticated' e estende a
// reserva UMA vez por grace. Recusa sessão já vinculada a outro subject.
func BindSession(ctx context.Context, tx pgx.Tx, sessionID, subjectID uuid.UUID, grace time.Duration) error {
	var s Session
	if err := lockSession(ctx, tx, sessionID, &s); err != nil {
		return err
	}
	if s.Status == "paid" || s.Status == "expired" || s.Status == "abandoned" {
		return ErrSessionNotOpen
	}
	if s.SubjectID != nil && *s.SubjectID != subjectID {
		return ErrSessionBoundToOther
	}
	exp := time.Now().Add(SessionAuthedTTL)
	if !s.GraceApplied {
		exp = exp.Add(grace)
		if s.HoldID != nil {
			if _, err := tx.Exec(ctx, `
				UPDATE seat_occupancy SET expires_at = expires_at + make_interval(secs => $2)
				 WHERE hold_id = $1 AND kind='hold' AND NOT released`, s.HoldID, grace.Seconds()); err != nil {
				return err
			}
		}
	}
	_, err := tx.Exec(ctx, `
		UPDATE checkout_sessions
		   SET subject_id=$2, status='authenticated', grace_applied=true, expires_at=$3, updated_at=now()
		 WHERE id=$1`, sessionID, subjectID, exp)
	return err
}

// PaySession cria a ordem/pagamento a partir da reserva já feita da sessão e marca 'paid'.
// Só paga sessão vinculada ao subject do token (validado pelo handler).
func PaySession(ctx context.Context, tx pgx.Tx, gw payment.PaymentGateway, prod Producer, s Session, req Request) (Result, error) {
	if req.Method != payment.MethodPix && req.Method != payment.MethodCard {
		return Result{}, ErrBadRequest
	}
	req.EventID = s.EventID
	req.LotID = s.Items.LotID
	req.Quantity = s.Items.Quantity
	req.SeatIDs = s.Items.SeatIDs
	req.HalfPriceQty = s.Items.HalfPriceQty
	req.CouponCode = s.Items.CouponCode
	req.CampaignID = s.Items.CampaignID
	lot, err := catalog.GetLot(ctx, tx, s.Items.LotID)
	if err != nil {
		return Result{}, err
	}
	res, err := finalizeOrder(ctx, tx, gw, prod, req, lot, s.HoldID)
	if err != nil {
		return Result{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE checkout_sessions SET status='paid', updated_at=now() WHERE id=$1`, s.ID); err != nil {
		return Result{}, err
	}
	return res, nil
}

// ExpireOpenSessions expira sessões abertas vencidas e libera a reserva (hold ou lote).
// Devolve quantas expirou. Roda sob tenancy.WithTenant.
func ExpireOpenSessions(ctx context.Context, tx pgx.Tx) (int, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, items, hold_id FROM checkout_sessions
		 WHERE status IN ('open','authenticated') AND expires_at <= now()
		 FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return 0, err
	}
	type row struct {
		id     uuid.UUID
		items  json.RawMessage
		holdID *uuid.UUID
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.items, &r.holdID); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range list {
		var items SessionItems
		_ = json.Unmarshal(r.items, &items)
		if r.holdID != nil {
			if err := inventory.Release(ctx, tx, *r.holdID); err != nil {
				return 0, err
			}
		} else if items.LotID != uuid.Nil {
			if err := releaseLot(ctx, tx, items.LotID, items.Quantity); err != nil {
				return 0, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE checkout_sessions SET status='expired', updated_at=now() WHERE id=$1`, r.id); err != nil {
			return 0, err
		}
	}
	return len(list), nil
}

// SessionSweeper expira sessões abertas vencidas de todos os produtores, periodicamente.
type SessionSweeper struct {
	pool     *pgxpool.Pool
	interval time.Duration
}

// NewSessionSweeper constrói a varredura de sessões.
func NewSessionSweeper(pool *pgxpool.Pool) *SessionSweeper {
	return &SessionSweeper{pool: pool, interval: 30 * time.Second}
}

// Run varre até o contexto encerrar.
func (sw *SessionSweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(sw.interval)
	defer ticker.Stop()
	for {
		if err := sw.processAll(ctx); err != nil {
			slog.Warn("checkout session sweeper", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (sw *SessionSweeper) processAll(ctx context.Context) error {
	schemas, err := tenancy.ListTenantSchemas(ctx, sw.pool)
	if err != nil {
		return err
	}
	for _, tid := range schemas {
		if err := sw.processTenant(ctx, tid); err != nil {
			slog.Warn("checkout session sweeper tenant", "tenant", tid, "err", err)
		}
	}
	return nil
}

func (sw *SessionSweeper) processTenant(ctx context.Context, tenantID uuid.UUID) error {
	tx, err := sw.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, tenantID); err != nil {
		return err
	}
	if _, err := ExpireOpenSessions(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CountOpenByIP conta sessões abertas (open|authenticated) de um IP.
func CountOpenByIP(ctx context.Context, tx pgx.Tx, ip string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM checkout_sessions
		 WHERE client_ip=$1 AND status IN ('open','authenticated')`, ip).Scan(&n)
	return n, err
}

// lockSession carrega a sessão FOR UPDATE.
func lockSession(ctx context.Context, tx pgx.Tx, id uuid.UUID, out *Session) error {
	s, err := loadSession(ctx, tx, sessionCols+` WHERE id=$1 FOR UPDATE`, id)
	if err != nil {
		return err
	}
	*out = s
	return nil
}

// sessionCols é a lista fixa de colunas de checkout_sessions.
const sessionCols = `SELECT id, event_id, anon_token, subject_id, client_ip, items, hold_id, status, grace_applied, expires_at
                        FROM checkout_sessions`

func loadSession(ctx context.Context, tx pgx.Tx, sql string, arg any) (Session, error) {
	var s Session
	var raw []byte
	var subjectID *uuid.UUID
	err := tx.QueryRow(ctx, sql, arg).
		Scan(&s.ID, &s.EventID, &s.AnonToken, &subjectID, &s.ClientIP, &raw, &s.HoldID, &s.Status, &s.GraceApplied, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrSessionNotFound
	}
	if err != nil {
		return Session{}, err
	}
	s.SubjectID = subjectID
	if err := json.Unmarshal(raw, &s.Items); err != nil {
		return Session{}, fmt.Errorf("lê seleção da sessão: %w", err)
	}
	return s, nil
}

// releaseLot devolve a reserva de LOTE (held_count) de uma sessão expirada/atualizada.
func releaseLot(ctx context.Context, tx pgx.Tx, lotID uuid.UUID, qty int) error {
	_, err := tx.Exec(ctx, `
		UPDATE lots SET held_count = GREATEST(held_count - $1, 0), updated_at = now() WHERE id=$2`,
		qty, lotID)
	return err
}
