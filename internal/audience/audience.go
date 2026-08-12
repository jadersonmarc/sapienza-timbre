// Package audience é a Camada 3 (Fase 3): segmentação por presença REAL, consentimento
// granular e revogável, recompensa a quem autoriza, e comercialização de ALCANCE (nunca
// dado pessoal) ao patrocinador. Guardrails no código: o patrocinador jamais vê
// identidade; entrega só a quem consentiu; consentimento não é pedágio (recusar não
// restringe nada). Tudo em public (cross-produtor).
//
// PROVISÓRIO (sem valor comercial definido — isolado e reportado): rewardPerAttendance.
package audience

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// rewardPerAttendanceCents: crédito por presença ao consentir. PROVISÓRIO — "quanto mais
// a pessoa circula, mais vale o consentimento", sem valor definido pelo negócio.
const rewardPerAttendanceCents = 100

// Segment é um segmento de audiência.
type Segment struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// CreateSegment cria um segmento com uma definição (critério de presença). Ex.:
// {"min_attendances": 2}.
func CreateSegment(ctx context.Context, pool *pgxpool.Pool, name string, definition []byte) (Segment, error) {
	if len(definition) == 0 {
		definition = []byte("{}")
	}
	var s Segment
	err := pool.QueryRow(ctx, `INSERT INTO audience_segments (name, definition) VALUES ($1,$2::jsonb) RETURNING id, name`, name, string(definition)).Scan(&s.ID, &s.Name)
	return s, err
}

// RecomputeSegment reavalia a definição contra o histórico de PRESENÇA e popula os
// membros. Suporta {"min_attendances": N}. Devolve o tamanho do segmento.
func RecomputeSegment(ctx context.Context, pool *pgxpool.Pool, segmentID uuid.UUID) (int, error) {
	var minAtt int
	_ = pool.QueryRow(ctx, `SELECT COALESCE((definition->>'min_attendances')::int, 1) FROM audience_segments WHERE id=$1`, segmentID).Scan(&minAtt)
	if minAtt < 1 {
		minAtt = 1
	}
	if _, err := pool.Exec(ctx, `DELETE FROM segment_memberships WHERE segment_id=$1`, segmentID); err != nil {
		return 0, err
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO segment_memberships (segment_id, subject_id)
		SELECT $1, subject_id FROM attendance_records
		 WHERE subject_id IS NOT NULL
		 GROUP BY subject_id HAVING count(*) >= $2
		ON CONFLICT DO NOTHING`, segmentID, minAtt); err != nil {
		return 0, err
	}
	return SegmentSize(ctx, pool, segmentID)
}

// SegmentSize é o tamanho do segmento (auditável: o anunciante confere que o público
// existe, sem ver quem é).
func SegmentSize(ctx context.Context, pool *pgxpool.Pool, segmentID uuid.UUID) (int, error) {
	var n int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM segment_memberships WHERE segment_id=$1`, segmentID).Scan(&n)
	return n, err
}

// ConsentedSize é quantos membros CONSENTIRAM (o alcance vendável).
func ConsentedSize(ctx context.Context, pool *pgxpool.Pool, segmentID uuid.UUID) (int, error) {
	var n int
	err := pool.QueryRow(ctx, `
		SELECT count(*) FROM segment_memberships sm
		 WHERE sm.segment_id=$1 AND EXISTS(
			SELECT 1 FROM consents c
			 WHERE c.subject_id=sm.subject_id AND c.segment_id=$1 AND c.granted AND c.revoked_at IS NULL)`, segmentID).Scan(&n)
	return n, err
}

// SetConsent liga/desliga o consentimento do sujeito para um segmento (granular,
// revogável). Ao conceder, gera recompensa escalada pela circulação. Recusar/revogar não
// restringe em nada o uso da plataforma.
func SetConsent(ctx context.Context, pool *pgxpool.Pool, subjectID, segmentID uuid.UUID, granted bool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if !granted {
		// Revoga o consentimento vigente (sai do alcance na hora).
		_, err := tx.Exec(ctx, `UPDATE consents SET revoked_at=now() WHERE subject_id=$1 AND segment_id=$2 AND granted AND revoked_at IS NULL`, subjectID, segmentID)
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	// Já consente? idempotente.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM consents WHERE subject_id=$1 AND segment_id=$2 AND granted AND revoked_at IS NULL)`, subjectID, segmentID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return tx.Commit(ctx)
	}
	var consentID uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO consents (subject_id, segment_id, granted) VALUES ($1,$2,true) RETURNING id`, subjectID, segmentID).Scan(&consentID); err != nil {
		return err
	}
	// Recompensa = crédito escalado pela quantidade de presenças (provisório).
	var attendances int
	_ = tx.QueryRow(ctx, `SELECT count(*) FROM attendance_records WHERE subject_id=$1`, subjectID).Scan(&attendances)
	amount := int64(attendances) * rewardPerAttendanceCents
	if amount > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO consent_rewards (consent_id, kind, amount) VALUES ($1,'credito',$2)`, consentID, amount); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// SponsorCampaign é uma campanha de patrocinador sobre um segmento.
type SponsorCampaign struct {
	ID      uuid.UUID `json:"id"`
	Sponsor string    `json:"sponsor"`
	Segment uuid.UUID `json:"segment_id"`
}

// CreateSponsorCampaign cria a campanha (comercialização de alcance).
func CreateSponsorCampaign(ctx context.Context, pool *pgxpool.Pool, sponsor string, segmentID uuid.UUID, budget float64) (SponsorCampaign, error) {
	var c SponsorCampaign
	err := pool.QueryRow(ctx, `INSERT INTO sponsor_campaigns (sponsor, segment_id, budget, status) VALUES ($1,$2,$3,'active') RETURNING id, sponsor, segment_id`, sponsor, segmentID, budget).Scan(&c.ID, &c.Sponsor, &c.Segment)
	return c, err
}

// Deliver entrega a campanha ao alcance CONSENTIDO do segmento (só a quem consentiu).
// Nenhuma identidade é devolvida — só o número entregue. Idempotente.
func Deliver(ctx context.Context, pool *pgxpool.Pool, campaignID uuid.UUID) (int, error) {
	var segmentID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT segment_id FROM sponsor_campaigns WHERE id=$1`, campaignID).Scan(&segmentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, errors.New("audience: campanha inexistente")
		}
		return 0, err
	}
	tag, err := pool.Exec(ctx, `
		INSERT INTO campaign_deliveries (campaign_id, subject_id)
		SELECT $1, sm.subject_id
		  FROM segment_memberships sm
		 WHERE sm.segment_id=$2 AND EXISTS(
			SELECT 1 FROM consents c
			 WHERE c.subject_id=sm.subject_id AND c.segment_id=$2 AND c.granted AND c.revoked_at IS NULL)
		ON CONFLICT (campaign_id, subject_id) DO NOTHING`, campaignID, segmentID)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// Metrics é o painel do patrocinador: só números, ZERO identidade.
type Metrics struct {
	SegmentSize   int `json:"segment_size"`
	ConsentedSize int `json:"consented_size"`
	Delivered     int `json:"delivered"`
}

// CampaignMetrics devolve as métricas de entrega sem qualquer identidade individual.
func CampaignMetrics(ctx context.Context, pool *pgxpool.Pool, campaignID uuid.UUID) (Metrics, error) {
	var m Metrics
	var segmentID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT segment_id FROM sponsor_campaigns WHERE id=$1`, campaignID).Scan(&segmentID); err != nil {
		return m, err
	}
	m.SegmentSize, _ = SegmentSize(ctx, pool, segmentID)
	m.ConsentedSize, _ = ConsentedSize(ctx, pool, segmentID)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM campaign_deliveries WHERE campaign_id=$1`, campaignID).Scan(&m.Delivered)
	return m, nil
}
