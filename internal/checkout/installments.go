package checkout

// Regras do parcelamento no cartão. PROVISÓRIAS e isoladas — o teto e o piso por parcela
// são decisão comercial, não técnica, e mudam sem tocar em mais nada.
//
// Hoje o parcelamento é SEM JUROS para o comprador: o total é o mesmo em 1× ou em 12×, e o
// custo do parcelamento no gateway sai do repasse. Repassar juros exigiria decisão de preço
// e mudança na decomposição mostrada antes de confirmar.
const (
	// MaxInstallments é o teto de parcelas oferecidas.
	MaxInstallments = 12
	// MinInstallmentCents é o piso por parcela (o gateway recusa parcelas muito pequenas).
	MinInstallmentCents = 500
)

// MaxInstallmentsFor devolve em quantas vezes um valor pode ser dividido respeitando o piso
// por parcela. Serve tanto para a tela montar as opções quanto para o servidor conferir o
// que chegou — a mesma regra dos dois lados.
func MaxInstallmentsFor(totalCents int64) int {
	if totalCents <= 0 {
		return 1
	}
	n := int(totalCents / MinInstallmentCents)
	if n < 1 {
		return 1
	}
	if n > MaxInstallments {
		return MaxInstallments
	}
	return n
}

// ValidInstallments confere o número de parcelas pedido para um total. Fora da faixa, o
// pedido é recusado aqui em vez de virar recusa do gateway com o dinheiro em jogo.
func ValidInstallments(n int, totalCents int64) bool {
	if n <= 1 {
		return true
	}
	return n <= MaxInstallmentsFor(totalCents)
}
