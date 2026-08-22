package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-timbre/internal/catalog"
	"github.com/jadersonmarc/sapienza-timbre/internal/inventory"
	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
)

// Limits são os limites e TTLs da sessão de checkout — todos configuráveis por variável de
// ambiente (ver LimitsFromEnv); nada cravado em linha de código além do default.
type Limits struct {
	MaxOpenSessionsPerIP int
	MaxHeldSeatsPerIP    int
	AnonTTL              time.Duration
	AuthedTTL            time.Duration
	AuthGrace            time.Duration
	IPRetention          time.Duration
}

// DefaultLimits devolve os valores iniciais (provisórios, calibrados para operadora móvel
// via CGNAT: teto de sessões alto, e o que se protege é o estoque travado por IP).
func DefaultLimits() Limits {
	return Limits{
		MaxOpenSessionsPerIP: 50,
		MaxHeldSeatsPerIP:    20,
		AnonTTL:              15 * time.Minute,
		AuthedTTL:            60 * time.Minute,
		AuthGrace:            600 * time.Second,
		IPRetention:          7 * 24 * time.Hour,
	}
}

// LimitsFromEnv carrega os limites do ambiente (segundos para durações), com o default.
func LimitsFromEnv() Limits {
	l := DefaultLimits()
	secs := map[string]*time.Duration{
		"TIMBRE_CHECKOUT_ANON_TTL":       &l.AnonTTL,
		"TIMBRE_CHECKOUT_AUTH_TTL":       &l.AuthedTTL,
		"TIMBRE_CHECKOUT_AUTH_GRACE":     &l.AuthGrace,
		"TIMBRE_CHECKOUT_IP_RETENTION":   &l.IPRetention,
	}
	for k, d := range secs {
		if v := os.Getenv(k); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				*d = time.Duration(n) * time.Second
			}
		}
	}
	ints := map[string]*int{
		"TIMBRE_CHECKOUT_MAX_OPEN_SESSIONS": &l.MaxOpenSessionsPerIP,
		"TIMBRE_CHECKOUT_MAX_HELD_SEATS":    &l.MaxHeldSeatsPerIP,
	}
	for k, v := range ints {
		if s := os.Getenv(k); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				*v = n
			}
		}
	}
	return l
}

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
	// ErrTooManySeats: teto de assentos reservados por IP atingido (evento com assento).
	ErrTooManySeats = errors.New("checkout: muitos assentos reservados deste IP")
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

// CreateSession cria a sessão, reserva (lote + assentos) e devolve a sessão. O anon_token é
// fornecido pelo cliente (identifica o navegador): se já existe sessão viva no MESMO evento,
// devolve a existente (retomada); sessões do mesmo anon_token em OUTROS eventos são expiradas
// (liberando reservas) antes de contar o teto por IP.
func CreateSession(ctx context.Context, tx pgx.Tx, req Request, anonToken, clientIP string, limits Limits) (Session, error) {
	// Expira sessões abertas do mesmo anon_token em outros eventos (o comprador não se
	// tranca sozinho) e retoma a sessão viva do mesmo evento, se houver.
	if anonToken != "" {
		if err := expireOwnedSessions(ctx, tx, anonToken, req.EventID); err != nil {
			return Session{}, err
		}
		var existing Session
		existing, err := loadSession(ctx, tx, sessionCols+` WHERE anon_token=$1 AND event_id=$2
			AND status IN ('open','authenticated') AND expires_at > now() LIMIT 1`, anonToken, req.EventID)
		if err == nil {
			return existing, nil
		} else if !errors.Is(err, ErrSessionNotFound) {
			return Session{}, err
		}
	}

	// Protege o estoque travado, não a quantidade de sessões: teto grosseiro de sessões e,
	// em evento com assento marcado, teto de assentos reservados por IP.
	open, err := CountOpenByIP(ctx, tx, clientIP)
	if err != nil {
		return Session{}, err
	}
	if open >= limits.MaxOpenSessionsPerIP {
		logLimit(clientIP, req.EventID, "max_open_sessions", limits.MaxOpenSessionsPerIP)
		return Session{}, ErrTooManySessions
	}
	if len(req.SeatIDs) > 0 {
		held, err := CountHeldSeatsByIP(ctx, tx, clientIP)
		if err != nil {
			return Session{}, err
		}
		if held+req.Quantity > limits.MaxHeldSeatsPerIP {
			logLimit(clientIP, req.EventID, "max_held_seats", limits.MaxHeldSeatsPerIP)
			return Session{}, ErrTooManySeats
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
	var s Session
	s.EventID = req.EventID
	s.AnonToken = anonToken
	if s.AnonToken == "" {
		s.AnonToken = uuid.NewString()
	}
	s.ClientIP = clientIP
	s.Items = items
	s.HoldID = holdID
	s.Status = "open"
	s.ExpiresAt = time.Now().Add(limits.AnonTTL)
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
func UpdateSession(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID, req Request, limits Limits) (Session, error) {
	var s Session
	if err := lockSession(ctx, tx, sessionID, &s); err != nil {
		return Session{}, err
	}
	if s.Status != "open" {
		return Session{}, ErrSessionNotOpen
	}
	if err := releaseSessionReservation(ctx, tx, s); err != nil {
		return Session{}, err
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
	exp := time.Now().Add(limits.AnonTTL)
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

// AuthStarted é chamado quando o código de acesso é pedido (o trecho lento — esperar o
// e-mail, trocar de app): estende reserva e sessão UMA vez por grace (grace_applied).
func AuthStarted(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID, limits Limits) error {
	var s Session
	if err := lockSession(ctx, tx, sessionID, &s); err != nil {
		return err
	}
	if s.Status == "paid" || s.Status == "expired" || s.Status == "abandoned" {
		return ErrSessionNotOpen
	}
	if !s.GraceApplied {
		exp := time.Now().Add(limits.AuthGrace)
		if s.HoldID != nil {
			if _, err := tx.Exec(ctx, `
				UPDATE seat_occupancy SET expires_at = expires_at + make_interval(secs => $2)
				 WHERE hold_id = $1 AND kind='hold' AND NOT released`, s.HoldID, limits.AuthGrace.Seconds()); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE checkout_sessions SET grace_applied=true, expires_at=$2, updated_at=now() WHERE id=$1`,
			sessionID, exp); err != nil {
			return err
		}
	}
	return nil
}

// BindSession vincula a sessão ao comprador (subject), marca 'authenticated' e estende a
// reserva UMA vez por grace (se o auth-started ainda não a tiver aplicado). Recusa sessão já
// vinculada a outro subject.
func BindSession(ctx context.Context, tx pgx.Tx, sessionID, subjectID uuid.UUID, limits Limits) error {
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
	exp := time.Now().Add(limits.AuthedTTL)
	if !s.GraceApplied {
		exp = exp.Add(limits.AuthGrace)
		if s.HoldID != nil {
			if _, err := tx.Exec(ctx, `
				UPDATE seat_occupancy SET expires_at = expires_at + make_interval(secs => $2)
				 WHERE hold_id = $1 AND kind='hold' AND NOT released`, s.HoldID, limits.AuthGrace.Seconds()); err != nil {
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

// ExpireOpenSessions expira sessões abertas vencidas, libera a reserva (hold ou lote) e limpa
// o client_ip (dado pessoal — não serve mais após o encerramento). Sessão que JÁ foi
// vinculada ('authenticated') vira 'abandoned' (quem entrou e não pagou abandonou); sessão
// 'open' que nunca foi vinculada vira 'expired'. Roda sob tenancy.
func ExpireOpenSessions(ctx context.Context, tx pgx.Tx) (int, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, status, items, hold_id FROM checkout_sessions
		 WHERE status IN ('open','authenticated') AND expires_at <= now()
		 FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return 0, err
	}
	type row struct {
		id     uuid.UUID
		status string
		items  json.RawMessage
		holdID *uuid.UUID
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.status, &r.items, &r.holdID); err != nil {
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
		newStatus := "expired"
		if r.status == "authenticated" {
			newStatus = "abandoned"
		}
		if _, err := tx.Exec(ctx, `UPDATE checkout_sessions SET status=$2, client_ip=NULL, updated_at=now() WHERE id=$1`, r.id, newStatus); err != nil {
			return 0, err
		}
	}
	return len(list), nil
}

// PurgePaidIPs apaga o client_ip de sessões 'paid' após o período de retenção.
func PurgePaidIPs(ctx context.Context, tx pgx.Tx, retention time.Duration) error {
	_, err := tx.Exec(ctx, `
		UPDATE checkout_sessions SET client_ip=NULL
		 WHERE status='paid' AND updated_at <= now() - make_interval(secs => $1)`, retention.Seconds())
	return err
}

// expireOwnedSessions expira as sessões abertas do mesmo anon_token em outros eventos e
// libera as reservas (o comprador não se tranca sozinho acumulando sessões).
func expireOwnedSessions(ctx context.Context, tx pgx.Tx, anonToken string, exceptEvent uuid.UUID) error {
	rows, err := tx.Query(ctx, `
		SELECT id, items, hold_id FROM checkout_sessions
		 WHERE anon_token=$1 AND status IN ('open','authenticated') AND event_id <> $2
		 FOR UPDATE SKIP LOCKED`, anonToken, exceptEvent)
	if err != nil {
		return err
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
			return err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range list {
		var items SessionItems
		_ = json.Unmarshal(r.items, &items)
		if r.holdID != nil {
			if err := inventory.Release(ctx, tx, *r.holdID); err != nil {
				return err
			}
		} else if items.LotID != uuid.Nil {
			if err := releaseLot(ctx, tx, items.LotID, items.Quantity); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE checkout_sessions SET status='expired', client_ip=NULL, updated_at=now() WHERE id=$1`, r.id); err != nil {
			return err
		}
	}
	return nil
}

// releaseSessionReservation libera a reserva (hold ou lote) de uma sessão.
func releaseSessionReservation(ctx context.Context, tx pgx.Tx, s Session) error {
	if s.HoldID != nil {
		return inventory.Release(ctx, tx, *s.HoldID)
	}
	return releaseLot(ctx, tx, s.Items.LotID, s.Items.Quantity)
}

// SessionSweeper expira sessões abertas vencidas e purga client_ip de sessões pagas.
type SessionSweeper struct {
	pool     *pgxpool.Pool
	limits   Limits
	interval time.Duration
}

// NewSessionSweeper constrói a varredura de sessões.
func NewSessionSweeper(pool *pgxpool.Pool, limits Limits) *SessionSweeper {
	return &SessionSweeper{pool: pool, limits: limits, interval: 30 * time.Second}
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
	if err := PurgePaidIPs(ctx, tx, sw.limits.IPRetention); err != nil {
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

// CountHeldSeatsByIP conta assentos reservados (holds vivos) pelas sessões abertas de um IP.
func CountHeldSeatsByIP(ctx context.Context, tx pgx.Tx, ip string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM seat_occupancy o
		 JOIN checkout_sessions s ON s.hold_id = o.hold_id
		 WHERE s.client_ip=$1 AND o.kind='hold' AND NOT o.released
		   AND s.status IN ('open','authenticated')`, ip).Scan(&n)
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

func loadSession(ctx context.Context, tx pgx.Tx, sql string, args ...any) (Session, error) {
	var s Session
	var raw []byte
	var subjectID *uuid.UUID
	err := tx.QueryRow(ctx, sql, args...).
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

// logLimit registra o disparo de um limite (IP, evento e limite atingido) — a instrumentação
// precisa aparecer antes de o produtor reclamar.
func logLimit(ip string, eventID uuid.UUID, limit string, value int) {
	slog.Warn("checkout limit hit",
		"limit", limit, "ip", ip, "event_id", eventID, "limit_value", value)
}
