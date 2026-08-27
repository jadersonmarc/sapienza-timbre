package checkout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// LegalWithdrawalDays é o piso da janela de arrependimento: art. 49 do CDC, sete dias
// contados da compra para serviço contratado fora do estabelecimento. O produtor pode
// oferecer MAIS, nunca menos — e quem garante isso é o CHECK da tabela, não este valor.
const LegalWithdrawalDays = 7

// Quem absorve a tarifa que o gateway retém e não devolve no estorno.
const (
	FeeBearerPlatform = "platform"
	FeeBearerProducer = "producer"
)

// ErrPolicyBelowLegal é a tentativa de encurtar a janela de arrependimento abaixo do piso
// legal. Sobe como 400 para o produtor entender o que a regra é, em vez de ver um erro de
// banco.
var ErrPolicyBelowLegal = fmt.Errorf("%w: a janela de arrependimento não pode ser menor que %d dias (art. 49 do CDC)",
	ErrBadRequest, LegalWithdrawalDays)

// Policy é a promessa de estorno de um evento. Nada aqui é chumbado no código: o default
// abaixo só vale enquanto o produtor não configurar nada.
type Policy struct {
	EventID                       *uuid.UUID `json:"event_id"`
	WithdrawalWindowDays          int        `json:"withdrawal_window_days"`
	WithdrawalMinHoursBeforeEvent int        `json:"withdrawal_min_hours_before_event"`
	RefundGatewayFeeBearer        string     `json:"refund_gateway_fee_bearer"`
	ProducerDiscretionaryEnabled  bool       `json:"producer_discretionary_enabled"`
	DiscretionaryResponseHours    int        `json:"discretionary_response_hours"`
	CheckinBlocksRefund           bool       `json:"checkin_blocks_refund"`
	// Inherited diz que esta política veio do default do produtor, não do evento. O painel
	// precisa distinguir "o evento decidiu isso" de "o evento não decidiu nada".
	Inherited bool `json:"inherited"`
}

// DefaultPolicy é o que vale quando o produtor nunca configurou nada. Conservador de
// propósito: a janela legal, sem antecedência exigida, tarifa com a plataforma, e a
// liberalidade aberta — o produtor decide caso a caso em vez de a porta nascer fechada.
func DefaultPolicy() Policy {
	return Policy{
		WithdrawalWindowDays:          LegalWithdrawalDays,
		WithdrawalMinHoursBeforeEvent: 0,
		RefundGatewayFeeBearer:        FeeBearerPlatform,
		ProducerDiscretionaryEnabled:  true,
		DiscretionaryResponseHours:    72,
		CheckinBlocksRefund:           true,
	}
}

const policyColumns = `withdrawal_window_days, withdrawal_min_hours_before_event,
	refund_gateway_fee_bearer, producer_discretionary_enabled, discretionary_response_hours,
	checkin_blocks_refund`

// ResolvePolicy devolve a política aplicável ao evento: a dele, senão o default do
// produtor, senão o embutido. Uma consulta só, na ordem de precedência.
func ResolvePolicy(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (Policy, error) {
	p := DefaultPolicy()
	var found *uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT event_id, `+policyColumns+`
		  FROM refund_policies
		 WHERE event_id = $1 OR event_id IS NULL
		 -- o do evento primeiro: NULLS LAST faz o default do produtor ser o desempate
		 ORDER BY event_id NULLS LAST
		 LIMIT 1`, eventID).
		Scan(&found, &p.WithdrawalWindowDays, &p.WithdrawalMinHoursBeforeEvent,
			&p.RefundGatewayFeeBearer, &p.ProducerDiscretionaryEnabled,
			&p.DiscretionaryResponseHours, &p.CheckinBlocksRefund)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultPolicy(), nil
	}
	if err != nil {
		return p, err
	}
	p.EventID = found
	p.Inherited = found == nil
	return p, nil
}

// GetPolicy lê a política gravada de um evento (ou o default do produtor, com eventID
// nulo), sem herança — é o que o formulário do painel edita.
func GetPolicy(ctx context.Context, tx pgx.Tx, eventID *uuid.UUID) (Policy, bool, error) {
	p := DefaultPolicy()
	p.EventID = eventID
	var row pgx.Row
	if eventID == nil {
		row = tx.QueryRow(ctx, `SELECT `+policyColumns+` FROM refund_policies WHERE event_id IS NULL`)
	} else {
		row = tx.QueryRow(ctx, `SELECT `+policyColumns+` FROM refund_policies WHERE event_id = $1`, *eventID)
	}
	err := row.Scan(&p.WithdrawalWindowDays, &p.WithdrawalMinHoursBeforeEvent,
		&p.RefundGatewayFeeBearer, &p.ProducerDiscretionaryEnabled,
		&p.DiscretionaryResponseHours, &p.CheckinBlocksRefund)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

// SavePolicy grava a política do evento (ou o default do produtor, com eventID nulo).
// Valida o piso legal aqui para a mensagem ser inteligível; o CHECK da tabela continua
// sendo a garantia.
func SavePolicy(ctx context.Context, tx pgx.Tx, eventID *uuid.UUID, p Policy) error {
	if p.WithdrawalWindowDays < LegalWithdrawalDays {
		return ErrPolicyBelowLegal
	}
	if p.WithdrawalMinHoursBeforeEvent < 0 {
		return fmt.Errorf("%w: antecedência mínima não pode ser negativa", ErrBadRequest)
	}
	if p.DiscretionaryResponseHours <= 0 {
		return fmt.Errorf("%w: o prazo de resposta precisa ser positivo", ErrBadRequest)
	}
	if p.RefundGatewayFeeBearer != FeeBearerPlatform && p.RefundGatewayFeeBearer != FeeBearerProducer {
		return fmt.Errorf("%w: responsável pela tarifa inválido: %q", ErrBadRequest, p.RefundGatewayFeeBearer)
	}
	// O default do produtor não tem event_id, e ON CONFLICT não trabalha com índice
	// parcial sobre expressão — os dois caminhos são escritos separados, de propósito.
	if eventID == nil {
		if _, err := tx.Exec(ctx, `
			UPDATE refund_policies SET
			  withdrawal_window_days=$1, withdrawal_min_hours_before_event=$2,
			  refund_gateway_fee_bearer=$3, producer_discretionary_enabled=$4,
			  discretionary_response_hours=$5, checkin_blocks_refund=$6, updated_at=now()
			 WHERE event_id IS NULL`,
			p.WithdrawalWindowDays, p.WithdrawalMinHoursBeforeEvent, p.RefundGatewayFeeBearer,
			p.ProducerDiscretionaryEnabled, p.DiscretionaryResponseHours, p.CheckinBlocksRefund); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO refund_policies (event_id, `+policyColumns+`)
			SELECT NULL, $1,$2,$3,$4,$5,$6
			 WHERE NOT EXISTS (SELECT 1 FROM refund_policies WHERE event_id IS NULL)`,
			p.WithdrawalWindowDays, p.WithdrawalMinHoursBeforeEvent, p.RefundGatewayFeeBearer,
			p.ProducerDiscretionaryEnabled, p.DiscretionaryResponseHours, p.CheckinBlocksRefund)
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO refund_policies (event_id, `+policyColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (event_id) WHERE event_id IS NOT NULL DO UPDATE SET
		  withdrawal_window_days=EXCLUDED.withdrawal_window_days,
		  withdrawal_min_hours_before_event=EXCLUDED.withdrawal_min_hours_before_event,
		  refund_gateway_fee_bearer=EXCLUDED.refund_gateway_fee_bearer,
		  producer_discretionary_enabled=EXCLUDED.producer_discretionary_enabled,
		  discretionary_response_hours=EXCLUDED.discretionary_response_hours,
		  checkin_blocks_refund=EXCLUDED.checkin_blocks_refund,
		  updated_at=now()`,
		*eventID, p.WithdrawalWindowDays, p.WithdrawalMinHoursBeforeEvent, p.RefundGatewayFeeBearer,
		p.ProducerDiscretionaryEnabled, p.DiscretionaryResponseHours, p.CheckinBlocksRefund)
	return err
}

// WithinWithdrawal diz se a compra ainda está na janela de arrependimento e com a
// antecedência exigida. Devolve também o motivo de não estar, que é o que o comprador
// precisa ler antes de pedir — e o que a recusa precisa dizer.
func (p Policy) WithinWithdrawal(boughtAt time.Time, startsAt *time.Time, now time.Time) (bool, string) {
	deadline := boughtAt.AddDate(0, 0, p.WithdrawalWindowDays)
	if now.After(deadline) {
		return false, fmt.Sprintf("a janela de arrependimento de %d dias terminou em %s",
			p.WithdrawalWindowDays, deadline.Format("02/01/2006"))
	}
	if p.WithdrawalMinHoursBeforeEvent > 0 && startsAt != nil {
		limite := startsAt.Add(-time.Duration(p.WithdrawalMinHoursBeforeEvent) * time.Hour)
		if now.After(limite) {
			return false, fmt.Sprintf("a devolução automática exige %d horas de antecedência do evento",
				p.WithdrawalMinHoursBeforeEvent)
		}
	}
	return true, ""
}
