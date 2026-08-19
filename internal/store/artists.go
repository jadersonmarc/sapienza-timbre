package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Artist é uma linha de public.artists (catálogo global, cross-tenant).
type Artist struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	Bio       *string   `json:"bio,omitempty"`
	ImageURL  *string   `json:"image_url,omitempty"`
	Category  *string   `json:"category,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateArtist insere um artista no catálogo global.
func CreateArtist(ctx context.Context, db DBTX, name, slug string, bio, imageURL, category *string) (Artist, error) {
	var a Artist
	err := db.QueryRow(ctx, `
		INSERT INTO artists (name, slug, bio, image_url, category)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, name, slug, bio, image_url, category, status, created_at`,
		name, slug, bio, imageURL, category,
	).Scan(&a.ID, &a.Name, &a.Slug, &a.Bio, &a.ImageURL, &a.Category, &a.Status, &a.CreatedAt)
	return a, err
}

// GetArtist devolve um artista por id.
func GetArtist(ctx context.Context, db DBTX, id uuid.UUID) (Artist, error) {
	var a Artist
	err := db.QueryRow(ctx, `
		SELECT id, name, slug, bio, image_url, category, status, created_at
		  FROM artists WHERE id = $1`, id,
	).Scan(&a.ID, &a.Name, &a.Slug, &a.Bio, &a.ImageURL, &a.Category, &a.Status, &a.CreatedAt)
	return a, err
}

// ListArtists lista o catálogo global (ativos por padrão; all=true inclui suspensos).
func ListArtists(ctx context.Context, db DBTX, all bool) ([]Artist, error) {
	q := `SELECT id, name, slug, bio, image_url, category, status, created_at
	        FROM artists`
	if !all {
		q += ` WHERE status = 'active'`
	}
	q += ` ORDER BY name`
	rows, err := db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Artist
	for rows.Next() {
		var a Artist
		if err := rows.Scan(&a.ID, &a.Name, &a.Slug, &a.Bio, &a.ImageURL, &a.Category, &a.Status, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateArtist atualiza os campos editáveis do perfil do artista.
func UpdateArtist(ctx context.Context, db DBTX, id uuid.UUID, name string, bio, imageURL, category *string) error {
	_, err := db.Exec(ctx, `
		UPDATE artists SET name=$2, bio=$3, image_url=$4, category=$5, updated_at=now()
		 WHERE id=$1`,
		id, name, bio, imageURL, category)
	return err
}

// SetArtistStatus suspende/reativa um artista (moderação reativa).
func SetArtistStatus(ctx context.Context, db DBTX, id uuid.UUID, status string) error {
	_, err := db.Exec(ctx, `UPDATE artists SET status=$2, updated_at=now() WHERE id=$1`, id, status)
	return err
}
