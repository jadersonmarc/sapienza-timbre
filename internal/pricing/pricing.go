// Package pricing calcula a decomposição de preço do modelo "Sympla" (§4): o produtor
// recebe o VALOR DE FACE limpo; o comprador paga face + taxa de conveniência. A taxa de
// conveniência = taxa de plataforma (10% do face, menos o rebate do nível) + custo de
// processamento repassado à adquirência (varia por método).
//
// VALOR DEFINIDO: taxa de plataforma = 10% (§4.1).
// PROVISÓRIOS (não inventados — constantes isoladas, inertes até definição comercial):
//   - Custo de processamento por método (§4.4): sem número definido → 0 (inerte). Trocar
//     quando a adquirência (Asaas) informar o custo real por Pix e por cartão.
//   - Direção do rebate de nível (§4.4): interpretação de trabalho = o rebate REDUZ a taxa
//     de plataforma cobrada do comprador (barateia para o público do produtor). A alternativa
//     (produtor recebe a diferença como crédito) fica parametrizada em RebateReducesBuyerFee.
package pricing

import "math"

// PlatformFeePct é a taxa de plataforma sobre o valor de face. DEFINIDO (§4.1): 10%.
const PlatformFeePct = 10.0

// Custo de processamento por método — PROVISÓRIO (§4.4). Inerte (0) até a adquirência
// definir. Isolados para trocar num único lugar.
const (
	pixProcessingCents = int64(0)
	cardProcessingPct  = 0.0
)

// RebateReducesBuyerFee — PROVISÓRIO (§4.4). true = o rebate do nível reduz a taxa de
// plataforma cobrada do comprador. false = a plataforma cobra a taxa cheia do comprador e o
// produtor recebe a diferença como crédito (não implementado aqui; ver ledger quando definido).
const RebateReducesBuyerFee = true

// Method espelha os métodos do gateway.
const (
	MethodPix  = "pix"
	MethodCard = "credit_card"
)

// Breakdown é a decomposição do preço mostrada ao comprador e usada no razão.
type Breakdown struct {
	FaceCents           int64 `json:"face_cents"`            // repasse ao produtor (limpo)
	PlatformFeeCents    int64 `json:"platform_fee_cents"`    // receita da plataforma (líq. do rebate)
	ProcessingCents     int64 `json:"processing_cents"`      // repasse à adquirência
	ConvenienceFeeCents int64 `json:"convenience_fee_cents"` // = platform + processing
	TotalCents          int64 `json:"total_cents"`           // = face + conveniência (o que o comprador paga)
}

// Compute decompõe o preço para um dado valor de face, método e rebate do nível (em %).
// Cortesia / valor zero não geram taxa (§4.3).
func Compute(faceCents int64, method string, tierRebatePct float64) Breakdown {
	if faceCents <= 0 {
		return Breakdown{}
	}
	platform := int64(math.Round(float64(faceCents) * PlatformFeePct / 100))
	if RebateReducesBuyerFee && tierRebatePct > 0 {
		platform -= int64(math.Round(float64(platform) * tierRebatePct / 100))
	}
	if platform < 0 {
		platform = 0
	}
	var processing int64
	if method == MethodCard {
		processing = int64(math.Round(float64(faceCents) * cardProcessingPct / 100))
	} else {
		processing = pixProcessingCents
	}
	conv := platform + processing
	return Breakdown{
		FaceCents:           faceCents,
		PlatformFeeCents:    platform,
		ProcessingCents:     processing,
		ConvenienceFeeCents: conv,
		TotalCents:          faceCents + conv,
	}
}
