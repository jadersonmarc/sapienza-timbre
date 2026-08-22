package chain

import (
	"context"
	"errors"
	"strings"
)

// ErrNotImplemented: o BaseChainDriver está configurado mas a emissão ERC-1155 está
// DESATIVADA por desenho (o eixo on-chain hoje é prova por âncora, não posse por token).
// Dormente até uma eventual retomada — nada chama mint/transfer/burn.
var ErrNotImplemented = errors.New("chain: emissão ERC-1155 desativada (prova por âncora, não posse)")

// BaseChainDriver fala com a Base (ERC-1155). É a implementação real por trás da
// interface; o resto do sistema não sabe que ela existe. Só habilita quando há RPC e
// contrato configurados.
type BaseChainDriver struct {
	rpcURL   string
	contract string
}

// NewBase constrói o driver da Base.
func NewBase(rpcURL, contract string) *BaseChainDriver {
	return &BaseChainDriver{rpcURL: strings.TrimSpace(rpcURL), contract: strings.TrimSpace(contract)}
}

// Enabled: só opera com RPC e contrato definidos.
func (b *BaseChainDriver) Enabled() bool { return b.rpcURL != "" && b.contract != "" }

func (b *BaseChainDriver) Mint(context.Context, MintRequest) (MintResult, error) {
	return MintResult{}, ErrNotImplemented
}
func (b *BaseChainDriver) Transfer(context.Context, TransferRequest) (TransferResult, error) {
	return TransferResult{}, ErrNotImplemented
}
func (b *BaseChainDriver) Burn(context.Context, string) error { return ErrNotImplemented }
func (b *BaseChainDriver) Status(context.Context, string) (TokenStatus, error) {
	return TokenStatus{}, ErrNotImplemented
}
