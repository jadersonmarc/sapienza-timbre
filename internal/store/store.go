// Package store guarda os repositórios pgx à mão (sem sqlc, mesmo estilo da Margot).
// As tabelas do control plane vivem em `public`, sempre no search_path, então estas
// funções operam pelo pool ou por uma tx indistintamente (DBTX). As tabelas de
// operação por-produtor (tenant_<id>) chegam em etapas seguintes, sempre sob
// tenancy.WithTenant.
package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DBTX é o subconjunto de pgx usado pelo store — satisfeito por *pgxpool.Pool e por
// pgx.Tx.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Producer é uma linha de public.producers (o tenant).
type Producer struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	Tier          string    `json:"tier"`
	RetentionPct  float64   `json:"retention_pct"`
	Status        string    `json:"status"`
	AsaasWalletID *string   `json:"asaas_wallet_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// Collaborator é uma linha de public.collaborators (sem o hash de senha).
type Collaborator struct {
	ID             uuid.UUID `json:"id"`
	ProducerID     uuid.UUID `json:"producer_id"`
	Email          string    `json:"email"`
	IsOwner        bool      `json:"is_owner"`
	SessionVersion int       `json:"session_version"`
	CreatedAt      time.Time `json:"created_at"`
}

// CollaboratorAuth carrega o hash e os campos de sessão para o login.
type CollaboratorAuth struct {
	ID             uuid.UUID
	ProducerID     uuid.UUID
	PasswordHash   string
	IsOwner        bool
	SessionVersion int
}

// CreateProducer insere um produtor e devolve a linha criada.
func CreateProducer(ctx context.Context, tx DBTX, name string) (Producer, error) {
	var p Producer
	err := tx.QueryRow(ctx, `
		INSERT INTO producers (name)
		VALUES ($1)
		RETURNING id, name, tier, retention_pct, status, created_at`,
		name,
	).Scan(&p.ID, &p.Name, &p.Tier, &p.RetentionPct, &p.Status, &p.CreatedAt)
	return p, err
}

// CreateCollaborator insere um colaborador de um produtor.
func CreateCollaborator(ctx context.Context, tx DBTX, producerID uuid.UUID, email, passwordHash string, isOwner bool) (Collaborator, error) {
	var c Collaborator
	err := tx.QueryRow(ctx, `
		INSERT INTO collaborators (producer_id, email, password_hash, is_owner)
		VALUES ($1, $2, $3, $4)
		RETURNING id, producer_id, email, is_owner, session_version, created_at`,
		producerID, email, passwordHash, isOwner,
	).Scan(&c.ID, &c.ProducerID, &c.Email, &c.IsOwner, &c.SessionVersion, &c.CreatedAt)
	return c, err
}

// AddPermission concede uma permissão granular a um colaborador (idempotente).
func AddPermission(ctx context.Context, tx DBTX, collaboratorID uuid.UUID, permission string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO collaborator_permissions (collaborator_id, permission)
		VALUES ($1, $2)
		ON CONFLICT DO NOTHING`,
		collaboratorID, permission)
	return err
}

// FindCollaboratorsByEmail devolve os colaboradores com aquele e-mail (o mesmo
// e-mail pode existir em produtores diferentes; o login confere a senha em cada um).
func FindCollaboratorsByEmail(ctx context.Context, db DBTX, email string) ([]CollaboratorAuth, error) {
	rows, err := db.Query(ctx, `
		SELECT id, producer_id, password_hash, is_owner, session_version
		  FROM collaborators WHERE email = $1`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CollaboratorAuth
	for rows.Next() {
		var c CollaboratorAuth
		if err := rows.Scan(&c.ID, &c.ProducerID, &c.PasswordHash, &c.IsOwner, &c.SessionVersion); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCollaborator devolve um colaborador por id.
func GetCollaborator(ctx context.Context, db DBTX, id uuid.UUID) (Collaborator, error) {
	var c Collaborator
	err := db.QueryRow(ctx, `
		SELECT id, producer_id, email, is_owner, session_version, created_at
		  FROM collaborators WHERE id = $1`, id,
	).Scan(&c.ID, &c.ProducerID, &c.Email, &c.IsOwner, &c.SessionVersion, &c.CreatedAt)
	return c, err
}

// GetProducer devolve um produtor por id.
func GetProducer(ctx context.Context, db DBTX, id uuid.UUID) (Producer, error) {
	var p Producer
	err := db.QueryRow(ctx, `
		SELECT id, name, tier, retention_pct, status, asaas_wallet_id, created_at
		  FROM producers WHERE id = $1`, id,
	).Scan(&p.ID, &p.Name, &p.Tier, &p.RetentionPct, &p.Status, &p.AsaasWalletID, &p.CreatedAt)
	return p, err
}

// ListPermissions devolve as permissões granulares de um colaborador.
func ListPermissions(ctx context.Context, db DBTX, collaboratorID uuid.UUID) ([]string, error) {
	rows, err := db.Query(ctx, `
		SELECT permission FROM collaborator_permissions
		 WHERE collaborator_id = $1 ORDER BY permission`, collaboratorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListCollaborators lista os colaboradores de um produtor.
func ListCollaborators(ctx context.Context, db DBTX, producerID uuid.UUID) ([]Collaborator, error) {
	rows, err := db.Query(ctx, `
		SELECT id, producer_id, email, is_owner, session_version, created_at
		  FROM collaborators WHERE producer_id = $1 ORDER BY created_at`, producerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Collaborator
	for rows.Next() {
		var c Collaborator
		if err := rows.Scan(&c.ID, &c.ProducerID, &c.Email, &c.IsOwner, &c.SessionVersion, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
