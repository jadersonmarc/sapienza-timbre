// Package wallet define o provedor de carteira invisível por MPC, criada na
// autenticação social, sem custódia de chave privada pela plataforma. Interface
// trocável; o provedor real chega junto com a rede (Etapa 1.8). Aqui só o seam.
package wallet

import "context"

// Wallet é uma carteira do público (endereço + custódia). O vínculo pessoa <->
// endereço mora em public.wallets; nada disso vai para payload de rede.
type Wallet struct {
	Address string
	Chain   string
	Custody string // "mpc"
}

// WalletProvider garante uma carteira para um sujeito (idempotente).
type WalletProvider interface {
	EnsureWallet(ctx context.Context, subjectID string) (Wallet, error)
}

// NoopWalletProvider é o default: não cria carteira nenhuma. Coerente com o guardrail
// de que a Camada 3 (identidade) nunca é obrigatória — quem só quer o ingresso não
// precisa de carteira.
type NoopWalletProvider struct{}

func (NoopWalletProvider) EnsureWallet(context.Context, string) (Wallet, error) {
	return Wallet{}, nil
}
