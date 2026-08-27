package attest

import (
	"context"
	"errors"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// LegalHalfPricePct é a cota mínima de meia-entrada: Lei 12.933/2013, art. 1º — 40% do
// total de ingressos disponíveis para cada evento. O produtor pode oferecer MAIS, nunca
// menos, e é por isso que o valor entra como PISO e não como default.
//
// A cota vale mesmo sem compromisso declarado: a obrigação é da lei, não da declaração.
const LegalHalfPricePct = 40.0

// ErrHalfPriceBelowLegal é a tentativa de declarar cota de meia abaixo do piso legal.
var ErrHalfPriceBelowLegal = errors.New("attest: a cota de meia-entrada não pode ser menor que 40% (Lei 12.933/2013, art. 1º)")

// ErrHalfPriceSoldOut é a cota de meia esgotada. A inteira continua à venda: acabou a
// meia, não o evento.
var ErrHalfPriceSoldOut = errors.New("attest: cota de meia-entrada esgotada")

// HalfPriceAllowance é o estado da cota de meia de um evento.
type HalfPriceAllowance struct {
	// Capacity é a base da cota: a soma das quantidades dos lotes (o total de ingressos
	// disponíveis do evento, que é o que a lei mede).
	Capacity  int `json:"capacity"`
	Quota     int `json:"quota"`
	Granted   int `json:"granted"`
	Remaining int `json:"remaining"`
	// Declared diz se o produtor declarou um compromisso próprio (acima do piso).
	Declared bool `json:"declared"`
}

// Available diz se ainda cabe meia-entrada.
func (a HalfPriceAllowance) Available() bool { return a.Remaining > 0 }

// HalfPrice calcula a cota aplicável, quanto já foi concedido e quanto resta.
//
// A cota declarada pelo produtor só vale quando é MAIOR que a legal — oferecer mais é
// direito dele, oferecer menos não é. E o compromisso declarado, que até aqui só era
// reportado no atestado, passa a valer na venda: uma cota que não barra nada é uma promessa
// sem consequência.
func HalfPrice(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (HalfPriceAllowance, error) {
	var a HalfPriceAllowance
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(quantity),0) FROM lots WHERE event_id=$1`, eventID).Scan(&a.Capacity); err != nil {
		return a, err
	}
	legal := int(math.Round(float64(a.Capacity) * LegalHalfPricePct / 100))
	a.Quota = legal

	var targetType, targetValue string
	err := tx.QueryRow(ctx, `
		SELECT target_type, target_value::text FROM event_commitments
		 WHERE event_id=$1 AND kind=$2 ORDER BY created_at LIMIT 1`, eventID, KindMeiaEntradaCota).
		Scan(&targetType, &targetValue)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Sem compromisso declarado: vale a lei.
	case err != nil:
		return a, err
	default:
		a.Declared = true
		if v, perr := parseValue(targetValue); perr == nil {
			declared := int(v)
			if targetType == TargetPercent {
				declared = int(math.Round(float64(a.Capacity) * v / 100))
			}
			a.Quota = max(a.Quota, declared)
		}
	}

	// Concedido é contado em INGRESSOS emitidos, não em compras: é assim que a lei mede, e
	// é o que faz um combo de duas meias consumir dois da cota, não um.
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM tickets
		 WHERE event_id=$1 AND half_price AND status IN ('active','used')`, eventID).Scan(&a.Granted); err != nil {
		return a, err
	}
	a.Remaining = max(a.Quota-a.Granted, 0)
	return a, nil
}

// EnsureHalfPrice recusa a venda que estouraria a cota. `qty` é quantas meias a compra
// pede. Chamado no funil de criação da ordem — a UI esconder o botão não basta.
func EnsureHalfPrice(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, qty int) error {
	if qty <= 0 {
		return nil
	}
	a, err := HalfPrice(ctx, tx, eventID)
	if err != nil {
		return err
	}
	if qty > a.Remaining {
		return ErrHalfPriceSoldOut
	}
	return nil
}
