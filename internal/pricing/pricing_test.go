package pricing

import (
	"testing"

	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
)

// tabela de tarifas de teste: valores plausíveis, mas o que importa é a RELAÇÃO — a sobra
// do Timbre tem de ser exatamente 10% do face em todo método e faixa.
func testFees() payment.Fees {
	return payment.Fees{
		Pix:       payment.MethodFee{Pct: 0.99, FixedCents: 0},
		Boleto:    payment.MethodFee{FixedCents: 199},
		DebitCard: payment.MethodFee{Pct: 1.89, FixedCents: 35},
		CreditCard: []payment.CreditTier{
			{MinInstallments: 1, MaxInstallments: 1, Pct: 2.99, FixedCents: 49},
			{MinInstallments: 2, MaxInstallments: 6, Pct: 3.49, FixedCents: 49},
			{MinInstallments: 7, MaxInstallments: 21, Pct: 3.99, FixedCents: 49},
		},
	}
}

// TestMargemEhSempreDezPorCentoDoFace: é a promessa do modelo. O produtor recebe o face
// limpo, o gateway leva a tarifa dele, e o que sobra para o Timbre é 10% do face —
// independentemente do método e do parcelamento.
func TestMargemEhSempreDezPorCentoDoFace(t *testing.T) {
	fees := testFees()
	faces := []int64{1000, 5000, 8790, 12345, 50000}
	casos := []struct {
		metodo   string
		parcelas int
	}{
		{payment.MethodPix, 1},
		{payment.MethodBoleto, 1},
		{payment.MethodDebit, 1},
		{payment.MethodCard, 1},
		{payment.MethodCard, 2},
		{payment.MethodCard, 6},
		{payment.MethodCard, 12},
		{payment.MethodCard, 21},
	}
	for _, face := range faces {
		for _, c := range casos {
			bd, err := Compute(face, c.metodo, c.parcelas, fees)
			if err != nil {
				t.Fatalf("%s %dx face %d: %v", c.metodo, c.parcelas, face, err)
			}
			tarifa, err := GatewayFeeCents(bd.TotalCents, c.metodo, c.parcelas, fees)
			if err != nil {
				t.Fatalf("tarifa: %v", err)
			}
			sobra := bd.TotalCents - face - tarifa
			esperado := face / 10
			// Folga de dois centavos: total e tarifa arredondam para cima, o que só pode
			// sobrar a MAIS para a plataforma, nunca a menos.
			if sobra < esperado || sobra > esperado+2 {
				t.Errorf("%s %dx face %d: sobra %d, esperava ~%d (total %d, tarifa %d)",
					c.metodo, c.parcelas, face, sobra, esperado, bd.TotalCents, tarifa)
			}
			if bd.TotalCents-tarifa < face {
				t.Errorf("%s %dx face %d: líquido %d abaixo do face", c.metodo, c.parcelas, face, bd.TotalCents-tarifa)
			}
			if bd.FaceCents != face || bd.TotalCents != face+bd.ConvenienceFeeCents {
				t.Errorf("decomposição inconsistente: %+v", bd)
			}
		}
	}
}

// TestFaixaDeParcelamento: o percentual do crédito muda por faixa, e o preço acompanha.
func TestFaixaDeParcelamento(t *testing.T) {
	fees := testFees()
	umaVez, _ := Compute(10000, payment.MethodCard, 1, fees)
	seisVezes, _ := Compute(10000, payment.MethodCard, 6, fees)
	dozeVezes, _ := Compute(10000, payment.MethodCard, 12, fees)
	if !(umaVez.TotalCents < seisVezes.TotalCents && seisVezes.TotalCents < dozeVezes.TotalCents) {
		t.Fatalf("o preço deveria subir com a faixa: 1x=%d 6x=%d 12x=%d",
			umaVez.TotalCents, seisVezes.TotalCents, dozeVezes.TotalCents)
	}
}

// TestArredondaParaCima: para baixo deixaria o líquido um centavo abaixo do face.
func TestArredondaParaCima(t *testing.T) {
	fees := payment.Fees{CreditCard: []payment.CreditTier{{MinInstallments: 1, MaxInstallments: 21, Pct: 3.33, FixedCents: 7}}}
	bd, err := Compute(3333, payment.MethodCard, 1, fees)
	if err != nil {
		t.Fatal(err)
	}
	exato := (float64(3333)*1.10 + 7) / (1 - 0.0333)
	if float64(bd.TotalCents) < exato {
		t.Fatalf("total %d ficou abaixo do valor exato %.4f", bd.TotalCents, exato)
	}
	if float64(bd.TotalCents) > exato+1 {
		t.Fatalf("total %d arredondou demais (exato %.4f)", bd.TotalCents, exato)
	}
}

// TestCortesiaNaoGeraTaxa: face zero não vira cobrança.
func TestCortesiaNaoGeraTaxa(t *testing.T) {
	bd, err := Compute(0, payment.MethodPix, 1, testFees())
	if err != nil || bd.TotalCents != 0 || bd.ConvenienceFeeCents != 0 {
		t.Fatalf("cortesia deveria sair zerada: %+v (%v)", bd, err)
	}
}

// TestTarifaDesconhecidaFalha: sem tarifa na tabela, o preço não pode ser arbitrado.
func TestTarifaDesconhecidaFalha(t *testing.T) {
	if _, err := Compute(10000, payment.MethodCard, 1, payment.Fees{}); err == nil {
		t.Fatal("crédito sem faixa na tabela deveria falhar em vez de arbitrar tarifa")
	}
	if _, err := Compute(10000, "pombo-correio", 1, testFees()); err == nil {
		t.Fatal("método desconhecido deveria falhar")
	}
}
