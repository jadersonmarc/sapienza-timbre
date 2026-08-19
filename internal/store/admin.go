package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Admin é uma linha de public.admins (operador da plataforma, sem o hash de senha).
type Admin struct {
	ID             uuid.UUID `json:"id"`
	Email          string    `json:"email"`
	Role           string    `json:"role"`
	SessionVersion int       `json:"session_version"`
	CreatedAt      time.Time `json:"created_at"`
}

// AdminAuth carrega o hash e os campos de sessão para o login do admin.
type AdminAuth struct {
	ID             uuid.UUID
	PasswordHash   string
	Role           string
	SessionVersion int
}

// FindAdminByEmail devolve o admin pelo e-mail (único globalmente).
func FindAdminByEmail(ctx context.Context, db DBTX, email string) (AdminAuth, error) {
	var a AdminAuth
	err := db.QueryRow(ctx, `
		SELECT id, password_hash, role, session_version
		  FROM admins WHERE email = $1`, email,
	).Scan(&a.ID, &a.PasswordHash, &a.Role, &a.SessionVersion)
	return a, err
}

// GetAdmin devolve um admin por id.
func GetAdmin(ctx context.Context, db DBTX, id uuid.UUID) (Admin, error) {
	var a Admin
	err := db.QueryRow(ctx, `
		SELECT id, email, role, session_version, created_at
		  FROM admins WHERE id = $1`, id,
	).Scan(&a.ID, &a.Email, &a.Role, &a.SessionVersion, &a.CreatedAt)
	return a, err
}

// ListAdmins lista todos os admins (super_admin only).
func ListAdmins(ctx context.Context, db DBTX) ([]Admin, error) {
	rows, err := db.Query(ctx, `
		SELECT id, email, role, session_version, created_at
		  FROM admins ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Admin
	for rows.Next() {
		var a Admin
		if err := rows.Scan(&a.ID, &a.Email, &a.Role, &a.SessionVersion, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CreateAdmin insere um admin (o primeiro super_admin é semeado no boot).
func CreateAdmin(ctx context.Context, db DBTX, email, passwordHash, role string) (Admin, error) {
	var a Admin
	err := db.QueryRow(ctx, `
		INSERT INTO admins (email, password_hash, role)
		VALUES ($1, $2, $3)
		RETURNING id, email, role, session_version, created_at`,
		email, passwordHash, role,
	).Scan(&a.ID, &a.Email, &a.Role, &a.SessionVersion, &a.CreatedAt)
	return a, err
}

// SetAdminRole muda o papel de um admin (super_admin only).
func SetAdminRole(ctx context.Context, db DBTX, id uuid.UUID, role string) error {
	_, err := db.Exec(ctx, `UPDATE admins SET role=$2, updated_at=now() WHERE id=$1`, id, role)
	return err
}

// BumpAdminSessionVersion invalida os JWTs emitidos antes da troca de senha.
func BumpAdminSessionVersion(ctx context.Context, db DBTX, id uuid.UUID) error {
	_, err := db.Exec(ctx, `UPDATE admins SET session_version = session_version + 1, updated_at=now() WHERE id=$1`, id)
	return err
}

// AuditEntry é uma linha de public.audit_log (trilha administrativa).
type AuditEntry struct {
	ID         uuid.UUID       `json:"id"`
	AdminID    *uuid.UUID      `json:"admin_id,omitempty"`
	Action     string          `json:"action"`
	EntityType *string         `json:"entity_type,omitempty"`
	EntityID   *uuid.UUID      `json:"entity_id,omitempty"`
	Details    json.RawMessage `json:"details"`
	CreatedAt  time.Time       `json:"created_at"`
}

// AppendAudit grava uma ação administrativa. details vira JSONB ({} se vazio).
func AppendAudit(ctx context.Context, db DBTX, adminID *uuid.UUID, action string, entityType *string, entityID *uuid.UUID, details map[string]any) error {
	raw, err := json.Marshal(details)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	_, err = db.Exec(ctx, `
		INSERT INTO audit_log (admin_id, action, entity_type, entity_id, details)
		VALUES ($1, $2, $3, $4, $5)`,
		adminID, action, entityType, entityID, raw)
	return err
}

// ListAuditLog devolve as ações mais recentes (limit <= 0 = 100).
func ListAuditLog(ctx context.Context, db DBTX, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.Query(ctx, `
		SELECT id, admin_id, action, entity_type, entity_id, details, created_at
		  FROM audit_log ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.AdminID, &e.Action, &e.EntityType, &e.EntityID, &e.Details, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
