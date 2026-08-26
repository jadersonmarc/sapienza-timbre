// Package fees serve a tabela de tarifas do gateway para o cálculo de preço. Três camadas,
// nessa ordem: memória (TTL curto), gateway e banco (última conhecida).
//
// A regra que organiza tudo: uma venda NUNCA trava por causa da tabela de tarifas, e
// tampouco calcula preço com tarifa arbitrada. Quando o gateway não responde, vale a última
// tabela persistida — e o log diz que estamos em contingência.
package fees

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
)

// CacheTTL é por quanto tempo a tabela em memória é considerada fresca. A tarifa muda por
// negociação comercial, não por minuto — consultar a cada venda só gastaria rate limit.
const CacheTTL = 6 * time.Hour

// Service entrega a tabela de tarifas vigente.
type Service struct {
	pool *pgxpool.Pool
	gw   payment.PaymentGateway

	mu       sync.RWMutex
	cached   payment.Fees
	cachedAt time.Time
}

// New constrói o serviço.
func New(pool *pgxpool.Pool, gw payment.PaymentGateway) *Service {
	return &Service{pool: pool, gw: gw}
}

// Current devolve a tabela vigente. Nunca devolve tabela vazia sem erro: preço sem tarifa
// conhecida é pior que venda recusada.
func (s *Service) Current(ctx context.Context) (payment.Fees, error) {
	s.mu.RLock()
	if s.cached.Complete() && time.Since(s.cachedAt) < CacheTTL {
		f := s.cached
		s.mu.RUnlock()
		return f, nil
	}
	s.mu.RUnlock()

	f, err := s.gw.Fees(ctx)
	if err == nil && f.Complete() {
		s.store(ctx, f)
		return f, nil
	}
	// Contingência: o gateway não respondeu (ou respondeu tabela incompleta). Vale a
	// última conhecida — e o operador precisa saber que estamos nela.
	stored, sErr := s.load(ctx)
	if sErr == nil && stored.Complete() {
		motivo := "tabela incompleta"
		if err != nil {
			motivo = err.Error()
		}
		slog.Warn("fees: operando com a última tabela conhecida (gateway indisponível)", "motivo", motivo)
		s.mu.Lock()
		s.cached, s.cachedAt = stored, time.Now()
		s.mu.Unlock()
		return stored, nil
	}
	if err != nil {
		return payment.Fees{}, fmt.Errorf("tabela de tarifas indisponível e sem valor persistido: %w", err)
	}
	return payment.Fees{}, fmt.Errorf("tabela de tarifas incompleta e sem valor persistido")
}

// Refresh força a releitura no gateway (boot e rota administrativa).
func (s *Service) Refresh(ctx context.Context) (payment.Fees, error) {
	f, err := s.gw.Fees(ctx)
	if err != nil {
		return payment.Fees{}, err
	}
	if !f.Complete() {
		return payment.Fees{}, fmt.Errorf("gateway devolveu tabela de tarifas incompleta")
	}
	s.store(ctx, f)
	return f, nil
}

func (s *Service) store(ctx context.Context, f payment.Fees) {
	s.mu.Lock()
	s.cached, s.cachedAt = f, time.Now()
	s.mu.Unlock()

	normalized, err := json.Marshal(f)
	if err != nil {
		return
	}
	raw := f.Raw
	if len(raw) == 0 || !json.Valid(raw) {
		raw = []byte(`{}`)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO asaas_fee_table (id, fees, raw, fetched_at) VALUES (1, $1, $2, now())
		ON CONFLICT (id) DO UPDATE SET fees=EXCLUDED.fees, raw=EXCLUDED.raw, fetched_at=now()`,
		normalized, raw); err != nil {
		slog.Warn("fees: não foi possível persistir a tabela", "err", err)
	}
}

func (s *Service) load(ctx context.Context) (payment.Fees, error) {
	var normalized, raw []byte
	var at time.Time
	if err := s.pool.QueryRow(ctx, `SELECT fees, COALESCE(raw,'{}'::jsonb), fetched_at FROM asaas_fee_table WHERE id=1`).
		Scan(&normalized, &raw, &at); err != nil {
		return payment.Fees{}, err
	}
	var f payment.Fees
	if err := json.Unmarshal(normalized, &f); err != nil {
		return payment.Fees{}, err
	}
	f.Raw = raw
	return f, nil
}
