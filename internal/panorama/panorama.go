// Package panorama monta o panorama de passeios do público (Etapa 2.5): mapa e linha
// do tempo dos lugares por onde a pessoa passou, e a retrospectiva anual. Leitura só,
// sobre public.attendance_records (cross-produtor) — nenhum dado pessoal exposto além
// do que o próprio dono compartilha.
package panorama

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Place é um lugar por onde a pessoa passou (um elo do mapa/linha do tempo).
type Place struct {
	EventTitle string   `json:"event_title"`
	Producer   string   `json:"producer"`
	Lat        *float64 `json:"lat,omitempty"`
	Lng        *float64 `json:"lng,omitempty"`
	OccurredAt string   `json:"occurred_at"`
}

// Places devolve os lugares do sujeito em ordem cronológica (mapa = mesma lista).
func Places(ctx context.Context, pool *pgxpool.Pool, subjectID uuid.UUID) ([]Place, error) {
	rows, err := pool.Query(ctx, `
		SELECT COALESCE(a.event_title,''), COALESCE(p.name,''), a.venue_lat, a.venue_lng,
		       to_char(a.occurred_at, 'YYYY-MM-DD"T"HH24:MI:SS')
		  FROM attendance_records a
		  LEFT JOIN producers p ON p.id = a.producer_id
		 WHERE a.subject_id = $1
		 ORDER BY a.occurred_at`, subjectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Place
	for rows.Next() {
		var pl Place
		if err := rows.Scan(&pl.EventTitle, &pl.Producer, &pl.Lat, &pl.Lng, &pl.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, pl)
	}
	return out, rows.Err()
}

// Retro é a retrospectiva anual (peça compartilhável).
type Retro struct {
	Year   int      `json:"year"`
	Events int      `json:"events"`
	Casas  int      `json:"casas"`
	Names  []string `json:"casas_list"`
}

// Retrospective agrega o ano do sujeito: eventos, casas distintas e a lista de casas.
func Retrospective(ctx context.Context, pool *pgxpool.Pool, subjectID uuid.UUID, year int) (Retro, error) {
	r := Retro{Year: year}
	if err := pool.QueryRow(ctx, `
		SELECT count(*), count(DISTINCT producer_id)
		  FROM attendance_records
		 WHERE subject_id=$1 AND EXTRACT(YEAR FROM occurred_at)=$2`, subjectID, year).Scan(&r.Events, &r.Casas); err != nil {
		return r, err
	}
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT COALESCE(p.name,'')
		  FROM attendance_records a LEFT JOIN producers p ON p.id=a.producer_id
		 WHERE a.subject_id=$1 AND EXTRACT(YEAR FROM a.occurred_at)=$2
		 ORDER BY 1`, subjectID, year)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return r, err
		}
		r.Names = append(r.Names, name)
	}
	return r, rows.Err()
}
