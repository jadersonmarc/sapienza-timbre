package checkout

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// LineupShare é a fatia informativa de um artista do line-up.
type LineupShare struct {
	ArtistName string  `json:"artist_name"`
	SharePct   float64 `json:"share_pct"`
	// AmountCents é o previsto sobre o face vendido — cálculo do painel, não movimento
	// de dinheiro: quem paga o artista é o produtor, por fora.
	AmountCents int64 `json:"amount_cents"`
}

// LineupPreview calcula o rateio previsto do line-up sobre o face LÍQUIDO vendido no
// evento. É informativo: nenhum artista é recebedor no gateway.
//
// O face líquido desconta o que foi devolvido POR INGRESSO. Antes a base vinha da tabela de
// repasses no gateway, que tinha uma linha por pedido: um estorno parcial ou tirava o
// pedido inteiro da conta ou não tirava nada, e o rateio previsto errava nos dois sentidos.
func LineupPreview(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) ([]LineupShare, error) {
	var faceSold int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE((SELECT SUM(face_cents) FROM orders
		                  WHERE event_id=$1 AND status IN ('paid','partially_refunded','refunded')),0)
		     - COALESCE((SELECT SUM(r.face_cents) FROM refunds r
		                   JOIN orders o ON o.id = r.order_id
		                  WHERE o.event_id=$1 AND r.status='confirmed'),0)`, eventID).Scan(&faceSold); err != nil {
		return nil, err
	}
	faceSold = max(faceSold, 0)
	rows, err := tx.Query(ctx, `
		SELECT artist_name, share_pct FROM lineup_shares WHERE event_id=$1 ORDER BY share_pct DESC`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LineupShare
	for rows.Next() {
		var s LineupShare
		if err := rows.Scan(&s.ArtistName, &s.SharePct); err != nil {
			return nil, err
		}
		s.AmountCents = int64(float64(faceSold) * s.SharePct / 100)
		out = append(out, s)
	}
	return out, rows.Err()
}
