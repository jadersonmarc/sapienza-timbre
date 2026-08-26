package payment

import (
	"encoding/json"
	"fmt"
	"math"
)

// Fees é a tabela de tarifas da conta no gateway, normalizada em centavos e pontos
// percentuais. Nunca é chumbada em constante: o preço do ingresso depende dela, e uma
// tarifa desatualizada aparece como split recusado por divergência na liquidação.
//
// Raw guarda a resposta original do gateway. É o que vai para o snapshot de auditoria da
// venda: se a conta mudar de plano, a única forma de explicar um preço antigo é ter a
// tabela como ela era naquele momento.
type Fees struct {
	Pix        MethodFee    `json:"pix"`
	Boleto     MethodFee    `json:"boleto"`
	DebitCard  MethodFee    `json:"debit_card"`
	CreditCard []CreditTier `json:"credit_card"`
	Raw        []byte       `json:"-"`
}

// MethodFee é a tarifa de um método: componente percentual (a) e fixo (b).
type MethodFee struct {
	Pct        float64 `json:"pct"`
	FixedCents int64   `json:"fixed_cents"`
}

// CreditTier é a tarifa do crédito numa faixa de parcelas. O percentual do crédito muda
// conforme o número de parcelas, então a faixa faz parte da tarifa.
type CreditTier struct {
	MinInstallments int     `json:"min_installments"`
	MaxInstallments int     `json:"max_installments"`
	Pct             float64 `json:"pct"`
	FixedCents      int64   `json:"fixed_cents"`
}

// For devolve a tarifa aplicável ao método e ao parcelamento. Faixa não encontrada é erro:
// calcular preço com tarifa arbitrada é o mesmo que inventar taxa.
func (f Fees) For(method string, installments int) (MethodFee, error) {
	switch method {
	case MethodPix:
		return f.Pix, nil
	case MethodBoleto:
		return f.Boleto, nil
	case MethodDebit:
		return f.DebitCard, nil
	case MethodCard:
		if installments < 1 {
			installments = 1
		}
		for _, t := range f.CreditCard {
			if installments >= t.MinInstallments && installments <= t.MaxInstallments {
				return MethodFee{Pct: t.Pct, FixedCents: t.FixedCents}, nil
			}
		}
		return MethodFee{}, fmt.Errorf("sem tarifa de crédito para %dx na tabela do gateway", installments)
	}
	return MethodFee{}, fmt.Errorf("método sem tarifa conhecida: %q", method)
}

// Complete diz se a tabela tem o mínimo para precificar. Tabela vazia costuma ser resposta
// inesperada do gateway (mudança de contrato), e usá-la sairia como preço sem tarifa.
func (f Fees) Complete() bool {
	return len(f.CreditCard) > 0
}

// asaasFeesResponse espelha GET /v3/myAccount/fees. Os nomes seguem a documentação do
// Asaas; campos ausentes ficam zerados e Complete() denuncia a tabela incompleta em vez de
// o preço sair errado silenciosamente.
type asaasFeesResponse struct {
	Payment struct {
		BankSlip struct {
			DefaultValue *float64 `json:"defaultValue"`
		} `json:"bankSlip"`
		CreditCard struct {
			OperationValue                   *float64 `json:"operationValue"`
			OneInstallmentPercentage         *float64 `json:"oneInstallmentPercentage"`
			UpToSixInstallmentsPercentage    *float64 `json:"upToSixInstallmentsPercentage"`
			UpToTwelveInstallmentsPercentage *float64 `json:"upToTwelveInstallmentsPercentage"`
		} `json:"creditCard"`
		DebitCard struct {
			OperationValue *float64 `json:"operationValue"`
			Percentage     *float64 `json:"percentage"`
		} `json:"debitCard"`
		Pix struct {
			FixedFeeValue   *float64 `json:"fixedFeeValue"`
			PercentageFee   *float64 `json:"percentageFee"`
			MinimumFeeValue *float64 `json:"minimumFeeValue"`
			MaximumFeeValue *float64 `json:"maximumFeeValue"`
		} `json:"pix"`
	} `json:"payment"`
}

// maxCreditInstallments é o teto de parcelas coberto pela última faixa de crédito. O
// gateway publica percentuais por faixa até doze; acima disso a própria API recusa o
// parcelamento, então estender a última faixa evita erro de "faixa não encontrada" antes
// de o gateway responder.
const maxCreditInstallments = 21

// parseAsaasFees normaliza a resposta do gateway. Percentuais vêm em pontos percentuais e
// valores em reais; aqui viram pontos percentuais e centavos.
func parseAsaasFees(raw []byte) (Fees, error) {
	var r asaasFeesResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return Fees{}, fmt.Errorf("ler tabela de taxas: %w", err)
	}
	f := Fees{Raw: raw}
	f.Pix = MethodFee{Pct: deref(r.Payment.Pix.PercentageFee), FixedCents: toCents(deref(r.Payment.Pix.FixedFeeValue))}
	f.Boleto = MethodFee{FixedCents: toCents(deref(r.Payment.BankSlip.DefaultValue))}
	f.DebitCard = MethodFee{
		Pct:        deref(r.Payment.DebitCard.Percentage),
		FixedCents: toCents(deref(r.Payment.DebitCard.OperationValue)),
	}

	op := toCents(deref(r.Payment.CreditCard.OperationValue))
	one := r.Payment.CreditCard.OneInstallmentPercentage
	six := r.Payment.CreditCard.UpToSixInstallmentsPercentage
	twelve := r.Payment.CreditCard.UpToTwelveInstallmentsPercentage
	if one != nil {
		f.CreditCard = append(f.CreditCard, CreditTier{MinInstallments: 1, MaxInstallments: 1, Pct: *one, FixedCents: op})
	}
	if six != nil {
		f.CreditCard = append(f.CreditCard, CreditTier{MinInstallments: 2, MaxInstallments: 6, Pct: *six, FixedCents: op})
	}
	if twelve != nil {
		// A última faixa publicada cobre até o teto que a API aceita parcelar.
		f.CreditCard = append(f.CreditCard, CreditTier{MinInstallments: 7, MaxInstallments: maxCreditInstallments, Pct: *twelve, FixedCents: op})
	}
	return f, nil
}

func deref(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

// toCents converte reais em centavos sem passar por aritmética de float no dinheiro final.
func toCents(v float64) int64 {
	return int64(math.Round(v * 100))
}
