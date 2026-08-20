// Package wallet define o endereço do participante e o provedor de carteira invisível.
// Com a emissão sob demanda, o endereço passa a ser DERIVADO deterministicamente da
// semente hierárquica (sem fornecedor de carteira/MPC), pelo próximo derivation_index.
// A semente vive no cofre (referenciada por CHAIN_HD_SEED_REF — nunca em env/arquivo).
package wallet

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Wallet é uma carteira do público (endereço + custódia). O vínculo pessoa <-> endereço
// mora em public.wallets; nada disso vai para payload de rede.
type Wallet struct {
	Address string
	Chain   string
	Custody string // "mpc"
	Origin  string // "derived" | "imported"
}

// WalletProvider garante uma carteira para um sujeito (idempotente). Seam legado —
// mantido para os fluxos que ainda não derivam (não é mais o caminho principal).
type WalletProvider interface {
	EnsureWallet(ctx context.Context, subjectID string) (Wallet, error)
}

// NoopWalletProvider é o default: não cria carteira nenhuma. Coerente com o guardrail de
// que a Camada 3 (identidade) nunca é obrigatória.
type NoopWalletProvider struct{}

func (NoopWalletProvider) EnsureWallet(context.Context, string) (Wallet, error) {
	return Wallet{}, nil
}

// ── derivação determinística ──────────────────────────────────────────────────

// SeedProvider resolve a semente hierárquica. A implementação real busca no cofre a partir
// de CHAIN_HD_SEED_REF; a semente nunca está em env nem em arquivo do repo.
type SeedProvider interface {
	Seed(ctx context.Context) ([]byte, error)
}

// ErrSeedUnavailable: a semente não está acessível (cofre ainda não ligado).
var ErrSeedUnavailable = errors.New("wallet: semente indisponível (cofre não ligado)")

// StaticSeedProvider injeta uma semente fixa (testes/dev local).
type StaticSeedProvider struct{ SeedBytes []byte }

func (s StaticSeedProvider) Seed(context.Context) ([]byte, error) { return s.SeedBytes, nil }

// VaultSeedProvider resolve a semente pela referência do cofre. PROVISÓRIO: o fetch real
// do cofre não está implementado — falha fechado até a integração.
type VaultSeedProvider struct{ Ref string }

func (VaultSeedProvider) Seed(context.Context) ([]byte, error) { return nil, ErrSeedUnavailable }

// ErrInvalidSignature: a assinatura de propriedade não confere (ou não foi verificável).
var ErrInvalidSignature = errors.New("wallet: assinatura de propriedade inválida")

// MessageVerifier verifica que o participante controla o endereço importado.
type MessageVerifier interface {
	Verify(address, message, signature string) bool
}

type rejectVerifier struct{}

func (rejectVerifier) Verify(string, string, string) bool { return false }

// Deriver deriva e importa endereços de participantes.
type Deriver struct {
	seeds    SeedProvider
	verifier MessageVerifier
}

// NewDeriver constrói o derivador. Verifier default rejeita (fail-closed).
func NewDeriver(sp SeedProvider, v MessageVerifier) *Deriver {
	if v == nil {
		v = rejectVerifier{}
	}
	return &Deriver{seeds: sp, verifier: v}
}

// DeriveAddress deriva o endereço do participante pelo próximo derivation_index (global,
// nunca reaproveitado) e grava com origin='derived'. Idempotente por subject.
func (d *Deriver) DeriveAddress(ctx context.Context, tx pgx.Tx, subjectID uuid.UUID) (Wallet, error) {
	var w Wallet
	err := tx.QueryRow(ctx, `
		SELECT address, chain, custody, origin FROM wallets
		 WHERE subject_id=$1 AND origin='derived' LIMIT 1`, subjectID).
		Scan(&w.Address, &w.Chain, &w.Custody, &w.Origin)
	if err == nil {
		return w, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Wallet{}, err
	}
	seed, err := d.seeds.Seed(ctx)
	if err != nil {
		return Wallet{}, err
	}
	var idx int64
	if err := tx.QueryRow(ctx, `SELECT nextval('wallet_derivation_seq')`).Scan(&idx); err != nil {
		return Wallet{}, err
	}
	addr := deriveAddress(seed, idx)
	if err := tx.QueryRow(ctx, `
		INSERT INTO wallets (subject_id, address, chain, custody, origin, derivation_index)
		VALUES ($1,$2,'base','mpc','derived',$3)
		RETURNING address, chain, custody, origin`, subjectID, addr, idx).
		Scan(&w.Address, &w.Chain, &w.Custody, &w.Origin); err != nil {
		return Wallet{}, err
	}
	return w, nil
}

// ImportAddress grava um endereço trazido pelo participante (origin='imported'), verificando
// a assinatura de propriedade da mensagem. Nenhuma chave privada trafega.
func (d *Deriver) ImportAddress(ctx context.Context, tx pgx.Tx, subjectID uuid.UUID, address, signature string) (Wallet, error) {
	if address == "" || signature == "" {
		return Wallet{}, ErrInvalidSignature
	}
	if !d.verifier.Verify(address, importMessage(address), signature) {
		return Wallet{}, ErrInvalidSignature
	}
	var w Wallet
	if err := tx.QueryRow(ctx, `
		INSERT INTO wallets (subject_id, address, chain, custody, origin)
		VALUES ($1,$2,'base','external','imported')
		ON CONFLICT (address) DO UPDATE SET subject_id = EXCLUDED.subject_id
		RETURNING address, chain, custody, origin`, subjectID, address).
		Scan(&w.Address, &w.Chain, &w.Custody, &w.Origin); err != nil {
		return Wallet{}, err
	}
	return w, nil
}

// deriveAddress deriva o endereço da semente + índice. PROVISÓRIO: não é BIP32/BIP44 —
// é sha256(semente || índice) truncado a 20 bytes. Determinístico e único por índice;
// trocar por BIP44 (m/44'/60'/0'/0/index) quando a derivação real for definida.
func deriveAddress(seed []byte, index int64) string {
	h := sha256.New()
	h.Write(seed)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(index))
	h.Write(b[:])
	sum := h.Sum(nil)
	return "0x" + hex.EncodeToString(sum[:20])
}

// importMessage é a mensagem que o participante assina ao importar o endereço. PROVISÓRIO:
// formato simples até definir o contrato de assinatura (EIP-191) junto com a rede.
func importMessage(address string) string {
	return "Timbre: autorizo este endereço (" + address + ")"
}
