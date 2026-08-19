package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ModerationFlag é uma linha de public.moderation_flags (denúncia em análise).
type ModerationFlag struct {
	ID         uuid.UUID  `json:"id"`
	TargetType string     `json:"target_type"`
	TargetID   uuid.UUID  `json:"target_id"`
	Reason     string     `json:"reason"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy *uuid.UUID `json:"resolved_by,omitempty"`
}

// CreateModerationFlag registra uma denúncia (moderação reativa).
func CreateModerationFlag(ctx context.Context, db DBTX, targetType string, targetID uuid.UUID, reason string) (ModerationFlag, error) {
	var f ModerationFlag
	err := db.QueryRow(ctx, `
		INSERT INTO moderation_flags (target_type, target_id, reason)
		VALUES ($1, $2, $3)
		RETURNING id, target_type, target_id, reason, status, created_at, resolved_at, resolved_by`,
		targetType, targetID, reason,
	).Scan(&f.ID, &f.TargetType, &f.TargetID, &f.Reason, &f.Status, &f.CreatedAt, &f.ResolvedAt, &f.ResolvedBy)
	return f, err
}

// ListModerationFlags lista a fila por status (vazio = todas).
func ListModerationFlags(ctx context.Context, db DBTX, status string) ([]ModerationFlag, error) {
	var (
		rows pgx.Rows
		err  error
	)
	q := `SELECT id, target_type, target_id, reason, status, created_at, resolved_at, resolved_by
	        FROM moderation_flags`
	if status != "" {
		rows, err = db.Query(ctx, q+` WHERE status = $1 ORDER BY created_at DESC`, status)
	} else {
		rows, err = db.Query(ctx, q+` ORDER BY created_at DESC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModerationFlag
	for rows.Next() {
		var f ModerationFlag
		if err := rows.Scan(&f.ID, &f.TargetType, &f.TargetID, &f.Reason, &f.Status, &f.CreatedAt, &f.ResolvedAt, &f.ResolvedBy); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ResolveModerationFlag resolve uma denúncia (resolved ou dismissed) marcando o admin.
func ResolveModerationFlag(ctx context.Context, db DBTX, id uuid.UUID, status string, adminID uuid.UUID) error {
	_, err := db.Exec(ctx, `
		UPDATE moderation_flags
		   SET status=$2, resolved_at=now(), resolved_by=$3
		 WHERE id=$1`, id, status, adminID)
	return err
}
