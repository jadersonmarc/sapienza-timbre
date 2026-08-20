// Package chain define a interface com a rede (Base/ERC-1155), no mesmo espírito do
// WhatsAppDriver da Margot: driver escolhido por config, implementação trocável sem
// tocar no chamador. Todo o sistema fala com a interface e não sabe se existe rede
// por trás. Default até a Etapa 1.8 é o NoopChainDriver.
package chain

import "context"

// MintRequest descreve a emissão de um token. Nenhum dado pessoal — só identificador,
// dono (endereço), lote e valor de face (ver guardrails).
type MintRequest struct {
	TokenID   string // por lote (pista) ou evento+setor+assento (lugar marcado)
	ToAddress string
	Amount    int64 // emissão em lote só se aplica à pista
	FaceValue int64
}

// MintResult é o resultado de uma emissão.
type MintResult struct {
	TxHash     string
	TokenID    string
	GasCostWei int64 // gás medido na testnet/rede (0 = não medido)
}

// TransferRequest descreve uma transferência restrita (pelo contrato da plataforma).
type TransferRequest struct {
	TokenID     string
	FromAddress string
	ToAddress   string
	PriceCents  int64
}

// TransferResult é o resultado de uma transferência.
type TransferResult struct {
	TxHash       string
	RoyaltyCents int64
}

// TokenStatus é o estado on-chain de um token.
type TokenStatus struct {
	TokenID string
	Owner   string
	Minted  bool
}

// MintMode controla QUANDO a emissão on-chain acontece. on_demand (default) materializa só
// quando a posse precisa se mover na cadeia; eager emite na confirmação do pagamento
// (comportamento anterior).
type MintMode string

const (
	// MintModeOnDemand: ingresso nasce 'not_materialized' e só vira 'pending' sob demanda.
	MintModeOnDemand MintMode = "on_demand"
	// MintModeEager: emissão enfileirada já na confirmação do pagamento.
	MintModeEager MintMode = "eager"
)

// ValidMintMode diz se m é um modo conhecido.
func ValidMintMode(m string) bool {
	return m == string(MintModeOnDemand) || m == string(MintModeEager)
}

// ChainDriver é a interface única com a rede. Enabled diz se há rede de verdade por
// trás — quando false, a emissão nem enfileira mint (chain_status fica 'none').
type ChainDriver interface {
	Enabled() bool
	Mint(ctx context.Context, req MintRequest) (MintResult, error)
	Transfer(ctx context.Context, req TransferRequest) (TransferResult, error)
	Burn(ctx context.Context, tokenID string) error
	Status(ctx context.Context, tokenID string) (TokenStatus, error)
}

// NoopChainDriver é o driver default: não fala com rede nenhuma. Deixa a venda e a
// entrada no evento totalmente independentes de RPC (guardrail: a rede nunca
// bloqueia a venda). A rede real (BaseChainDriver) liga quando configurada.
type NoopChainDriver struct{}

// Enabled é false: sem rede, a emissão não enfileira mint.
func (NoopChainDriver) Enabled() bool { return false }

func (NoopChainDriver) Mint(context.Context, MintRequest) (MintResult, error) {
	return MintResult{}, nil
}
func (NoopChainDriver) Transfer(context.Context, TransferRequest) (TransferResult, error) {
	return TransferResult{}, nil
}
func (NoopChainDriver) Burn(context.Context, string) error { return nil }
func (NoopChainDriver) Status(context.Context, string) (TokenStatus, error) {
	return TokenStatus{}, nil
}
