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
	TxHash  string
	TokenID string
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

// ChainDriver é a interface única com a rede.
type ChainDriver interface {
	Mint(ctx context.Context, req MintRequest) (MintResult, error)
	Transfer(ctx context.Context, req TransferRequest) (TransferResult, error)
	Burn(ctx context.Context, tokenID string) error
	Status(ctx context.Context, tokenID string) (TokenStatus, error)
}

// NoopChainDriver é o driver default: não fala com rede nenhuma. Deixa a venda e a
// entrada no evento totalmente independentes de RPC (guardrail: a rede nunca
// bloqueia a venda). A implementação real (BaseChainDriver) chega na Etapa 1.8.
type NoopChainDriver struct{}

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
