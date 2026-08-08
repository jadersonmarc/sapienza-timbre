// Package trust é descoberta e confiança (Etapa 2.6): avaliação restrita a quem fez
// check-in, reputação verificável do produtor e descoberta por presença real. Leitura/
// escrita em public (cross-produtor); a reputação recalcula sob tenancy.WithTenant.
package trust

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"
)

var (
	// ErrNotAttended: sem check-in correspondente — não pode avaliar.
	ErrNotAttended = errors.New("trust: avaliação restrita a quem fez check-in")
	// ErrAlreadyReviewed: já avaliou este check-in.
	ErrAlreadyReviewed = errors.New("trust: check-in já avaliado")
	// ErrBadRating: nota fora de 1..5.
	ErrBadRating = errors.New("trust: nota deve ser de 1 a 5")
)

// SubmitReview registra uma avaliação — só se existir uma presença (attendance) para o
// check-in informado. Deriva sujeito/produtor/evento da própria presença.
func SubmitReview(ctx context.Context, pool *pgxpool.Pool, checkinID uuid.UUID, rating int, body string) (uuid.UUID, error) {
	if rating < 1 || rating > 5 {
		return uuid.Nil, ErrBadRating
	}
	var subjectID, producerID, eventID *uuid.UUID
	err := pool.QueryRow(ctx, `SELECT subject_id, producer_id, event_id FROM attendance_records WHERE checkin_id=$1`, checkinID).
		Scan(&subjectID, &producerID, &eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotAttended
	}
	if err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err = pool.QueryRow(ctx, `
		INSERT INTO reviews (subject_id, producer_id, event_id, checkin_id, rating, body)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		subjectID, producerID, eventID, checkinID, rating, nilStr(body)).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return uuid.Nil, ErrAlreadyReviewed
		}
		return uuid.Nil, err
	}
	return id, nil
}

// Review é uma avaliação pública.
type Review struct {
	Rating int    `json:"rating"`
	Body   string `json:"body,omitempty"`
	At     string `json:"at"`
}

// ProducerReviews devolve as avaliações do produtor + a média.
func ProducerReviews(ctx context.Context, pool *pgxpool.Pool, producerID uuid.UUID) ([]Review, float64, error) {
	var avg float64
	_ = pool.QueryRow(ctx, `SELECT COALESCE(AVG(rating),0) FROM reviews WHERE producer_id=$1`, producerID).Scan(&avg)
	rows, err := pool.Query(ctx, `
		SELECT rating, COALESCE(body,''), to_char(created_at,'YYYY-MM-DD"T"HH24:MI:SS')
		  FROM reviews WHERE producer_id=$1 ORDER BY created_at DESC LIMIT 100`, producerID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Review
	for rows.Next() {
		var r Review
		if err := rows.Scan(&r.Rating, &r.Body, &r.At); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, avg, rows.Err()
}

// Reputation é a reputação verificável do produtor.
type Reputation struct {
	EventsDelivered  int     `json:"events_delivered"`
	CancellationRate float64 `json:"cancellation_rate"`
	RefundRate       float64 `json:"refund_rate"`
}

// RecomputeReputation recalcula a reputação a partir dos dados do produtor (eventos
// entregues, taxa de cancelamento, taxa de reembolso) e materializa em public.
func RecomputeReputation(ctx context.Context, pool *pgxpool.Pool, producerID uuid.UUID) (Reputation, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Reputation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, producerID); err != nil {
		return Reputation{}, err
	}
	var rep Reputation
	var totalEvents, cancelled int
	_ = tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='finished'), count(*), count(*) FILTER (WHERE status='cancelled') FROM events`).
		Scan(&rep.EventsDelivered, &totalEvents, &cancelled)
	if totalEvents > 0 {
		rep.CancellationRate = ratio(cancelled, totalEvents)
	}
	var paid, refunded int
	_ = tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='paid'), count(*) FILTER (WHERE status='refunded') FROM orders`).Scan(&paid, &refunded)
	if paid+refunded > 0 {
		rep.RefundRate = ratio(refunded, paid+refunded)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO public.producer_reputation (producer_id, events_delivered, cancellation_rate, refund_rate, updated_at)
		VALUES ($1,$2,$3,$4, now())
		ON CONFLICT (producer_id) DO UPDATE
		   SET events_delivered=EXCLUDED.events_delivered, cancellation_rate=EXCLUDED.cancellation_rate,
		       refund_rate=EXCLUDED.refund_rate, updated_at=now()`,
		producerID, rep.EventsDelivered, rep.CancellationRate, rep.RefundRate); err != nil {
		return Reputation{}, err
	}
	return rep, tx.Commit(ctx)
}

// Suggestion é um evento sugerido por presença real.
type Suggestion struct {
	EventTitle string `json:"event_title"`
	Producer   string `json:"producer"`
	Score      int    `json:"score"`
}

// Discovery sugere eventos por CO-PRESENÇA: quem esteve nos mesmos lugares que o sujeito
// foi a quais outros eventos (que o sujeito ainda não foi). Descoberta por presença
// real, não por cliques.
func Discovery(ctx context.Context, pool *pgxpool.Pool, subjectID uuid.UUID) ([]Suggestion, error) {
	rows, err := pool.Query(ctx, `
		SELECT a2.event_title, COALESCE(p.name,''), count(*) AS score
		  FROM attendance_records mine
		  JOIN attendance_records shared
		    ON shared.event_id = mine.event_id AND shared.subject_id <> mine.subject_id
		  JOIN attendance_records a2
		    ON a2.subject_id = shared.subject_id AND a2.event_id <> mine.event_id
		  LEFT JOIN producers p ON p.id = a2.producer_id
		 WHERE mine.subject_id = $1
		   AND a2.event_id NOT IN (SELECT event_id FROM attendance_records WHERE subject_id = $1)
		 GROUP BY a2.event_id, a2.event_title, p.name
		 ORDER BY score DESC, a2.event_title
		 LIMIT 10`, subjectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Suggestion
	for rows.Next() {
		var s Suggestion
		if err := rows.Scan(&s.EventTitle, &s.Producer, &s.Score); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(int(float64(a)/float64(b)*10000)) / 100 // 2 casas
}

func nilStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
