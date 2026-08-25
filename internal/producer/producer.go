// Package producer cria produtores (o tenant do Timbre) e provisiona o schema de
// operação de cada um. Diferente da Margot (que provisiona por evento de outbox do
// core), aqui a criação é síncrona: inserimos a linha de controle em `public` e,
// no ato, provisionamos tenant_<id> e rodamos as migrations de tenant.
package producer

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/store"
)

// Provisioner cria produtores e aplica as migrations de tenant.
type Provisioner struct {
	pool   *pgxpool.Pool
	runner *tenancy.MigrationRunner
}

// New constrói o provisionador.
func New(pool *pgxpool.Pool, runner *tenancy.MigrationRunner) *Provisioner {
	return &Provisioner{pool: pool, runner: runner}
}

// Result é o produtor criado com seu colaborador owner.
type Result struct {
	Producer store.Producer     `json:"producer"`
	Owner    store.Collaborator `json:"owner"`
}

// Create cria o produtor ATIVO + o colaborador owner e provisiona o schema tenant_<id>.
// O owner tem todas as permissões implicitamente (is_owner), então não gravamos
// linhas em collaborator_permissions para ele.
func (p *Provisioner) Create(ctx context.Context, name, ownerEmail, ownerPassword string) (Result, error) {
	return p.create(ctx, name, ownerEmail, ownerPassword, "active", "")
}

// CreateWithWallet cria o produtor ativo já com a carteira de recebimento informada no
// cadastro — é o caminho do cadastro público.
func (p *Provisioner) CreateWithWallet(ctx context.Context, name, ownerEmail, ownerPassword, asaasWalletID string) (Result, error) {
	return p.create(ctx, name, ownerEmail, ownerPassword, "active", asaasWalletID)
}

// CreatePending cria o produtor PENDENTE de aprovação (cadastro público da landing B2B).
// O owner já existe e pode logar, mas o produtor entra na fila de aprovação do admin.
func (p *Provisioner) CreatePending(ctx context.Context, name, ownerEmail, ownerPassword string) (Result, error) {
	return p.create(ctx, name, ownerEmail, ownerPassword, "pending", "")
}

func (p *Provisioner) create(ctx context.Context, name, ownerEmail, ownerPassword, status, asaasWalletID string) (Result, error) {
	hash, err := auth.HashPassword(ownerPassword)
	if err != nil {
		return Result{}, fmt.Errorf("hash de senha: %w", err)
	}
	email := strings.ToLower(strings.TrimSpace(ownerEmail))

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	prod, err := store.CreateProducerFull(ctx, tx, name, status, asaasWalletID)
	if err != nil {
		return Result{}, fmt.Errorf("criar produtor: %w", err)
	}
	owner, err := store.CreateCollaborator(ctx, tx, prod.ID, email, hash, true)
	if err != nil {
		return Result{}, fmt.Errorf("criar owner: %w", err)
	}
	// Cria o schema tenant_<id> na mesma transação da inserção de controle.
	if err := (tenancy.Provisioner{}).CreateSchema(ctx, tx, prod.ID); err != nil {
		return Result{}, fmt.Errorf("criar schema do produtor: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, err
	}

	// Fora da tx: aplica as migrations de tenant (cada uma na própria transação,
	// idempotente). Se falhar, o boot roda ApplyToAllTenants como catch-up.
	if err := p.runner.ApplyToTenant(ctx, prod.ID); err != nil {
		return Result{}, fmt.Errorf("migrar schema do produtor: %w", err)
	}
	return Result{Producer: prod, Owner: owner}, nil
}
