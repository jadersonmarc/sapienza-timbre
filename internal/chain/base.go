package chain

import (
	"context"
	"errors"
	"strings"
)

// ErrNotWired: o BaseChainDriver está configurado mas a emissão on-chain real ainda
// não foi ligada. A assinatura da transação EVM e o ERC-1155 dependem do contrato
// auditado (pré-requisito da Fase 2). Até lá, os jobs de mint acumulam com este erro
// registrado em chain_jobs.last_error — a venda e a portaria seguem intactas.
var ErrNotWired = errors.New("chain: BaseChainDriver ainda não ligado (contrato auditado — Fase 2)")

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
	return MintResult{}, ErrNotWired
}
func (b *BaseChainDriver) Transfer(context.Context, TransferRequest) (TransferResult, error) {
	return TransferResult{}, ErrNotWired
}
func (b *BaseChainDriver) Burn(context.Context, string) error { return ErrNotWired }
func (b *BaseChainDriver) Status(context.Context, string) (TokenStatus, error) {
	return TokenStatus{}, ErrNotWired
}
