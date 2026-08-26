// Package pricing calcula o preço do ingresso no modelo de face limpo: o produtor recebe
// exatamente o VALOR DE FACE, e o comprador paga face + conveniência. A margem do Timbre é
// a conveniência menos a tarifa do gateway.
//
// O cálculo é circular: a tarifa percentual do gateway incide sobre o valor cobrado, que já
// contém a conveniência. Resolvendo para V:
//
//	V = (F * (1 + p) + b) / (1 - a)
//
//	F = face          p = taxa de plataforma (10%)
//	a = % do gateway  b = fixo do gateway (ambos dependem do método e do parcelamento)
//
// Por isso o preço final só existe DEPOIS que o comprador escolhe como vai pagar.
package pricing

import (
	"fmt"
	"math"

	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
)

// PlatformFeePct é a taxa de plataforma sobre o face, igual para todo produtor. É
// configuração (um único ponto de mudança), não tabela de níveis: não há desconto por
// produtor.
const PlatformFeePct = 10.0

// Breakdown é a decomposição do preço, mostrada ao comprador e guardada na venda.
type Breakdown struct {
	FaceCents           int64 `json:"face_cents"`            // repasse ao produtor (limpo, é o split)
	PlatformFeeCents    int64 `json:"platform_fee_cents"`    // margem do Timbre (face × p)
	ProcessingCents     int64 `json:"processing_cents"`      // tarifa estimada do gateway
	ConvenienceFeeCents int64 `json:"convenience_fee_cents"` // = platform + processing (V − F)
	TotalCents          int64 `json:"total_cents"`           // = V, o que o comprador paga
}

// Compute resolve V para um face, método e parcelamento, usando a tabela de tarifas
// vigente. Erra em vez de arbitrar quando a tarifa do método não existe na tabela.
func Compute(faceCents int64, method string, installments int, fees payment.Fees) (Breakdown, error) {
	if faceCents <= 0 {
		return Breakdown{}, nil // cortesia não gera cobrança nem taxa
	}
	fee, err := fees.For(method, installments)
	if err != nil {
		return Breakdown{}, err
	}
	a := fee.Pct / 100
	if a >= 1 {
		return Breakdown{}, fmt.Errorf("tarifa percentual inválida (%.2f%%)", fee.Pct)
	}
	p := PlatformFeePct / 100

	// Arredonda o total SEMPRE para cima: um centavo a menos deixaria o líquido abaixo do
	// face e o gateway recusaria o split por divergência.
	numerador := float64(faceCents)*(1+p) + float64(fee.FixedCents)
	total := int64(math.Ceil(numerador / (1 - a)))

	platform := int64(math.Round(float64(faceCents) * p))
	conv := total - faceCents
	processing := conv - platform
	if processing < 0 {
		// Só acontece se a tarifa do gateway for maior que a conveniência inteira, o que
		// significaria vender no prejuízo. Melhor deixar explícito no número que esconder.
		processing = 0
	}
	return Breakdown{
		FaceCents:           faceCents,
		PlatformFeeCents:    platform,
		ProcessingCents:     processing,
		ConvenienceFeeCents: conv,
		TotalCents:          total,
	}, nil
}

// GatewayFeeCents estima a tarifa que o gateway vai reter de um valor cobrado. Serve para a
// asserção antes de criar a cobrança: o líquido precisa cobrir o face, senão o split é
// recusado na liquidação — semanas depois, com o dinheiro em jogo.
func GatewayFeeCents(totalCents int64, method string, installments int, fees payment.Fees) (int64, error) {
	fee, err := fees.For(method, installments)
	if err != nil {
		return 0, err
	}
	return int64(math.Ceil(float64(totalCents)*fee.Pct/100)) + fee.FixedCents, nil
}
