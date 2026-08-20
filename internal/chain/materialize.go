package chain

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-kit/tenancy"
)

// Razões conhecidas de materialização (espelham a CHECK de chain_jobs.reason).
const (
	ReasonExport        = "export"
	ReasonResaleListing = "resale_listing"
	ReasonCollectible   = "collectible"
	ReasonBulkProducer  = "bulk_producer"
	ReasonBackfill      = "backfill"
)

// DefaultBackfillLimit é o teto de ingressos materializados por execução do backfill.
// PROVISÓRIO — calibrar com o custo de gás medido na testnet.
const DefaultBackfillLimit = 100

// Materialize enfileira a materialização on-chain dos ingressos ainda em
// 'not_materialized' (ignora os já 'pending'/'minted'). Agrupa ingressos de pista
// (seat_id nulo) do MESMO lote num único job — o ERC-1155 é por lote; assento marcado não
// agrupa (um job por ingresso). Roda na MESMA transação do chamador.
func Materialize(ctx context.Context, tx pgx.Tx, ticketIDs []uuid.UUID, reason string) error {
	if len(ticketIDs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		SELECT id, lot_id, seat_id FROM tickets
		 WHERE id = ANY($1) AND chain_status = 'not_materialized'
		 FOR UPDATE`, ticketIDs)
	if err != nil {
		return err
	}
	type tk struct {
		id     uuid.UUID
		lotID  uuid.UUID
		seatID *uuid.UUID
	}
	var standing []tk // pista (agrupada por lote)
	var seated []tk   // assento marcado (individual)
	for rows.Next() {
		var t tk
		if err := rows.Scan(&t.id, &t.lotID, &t.seatID); err != nil {
			rows.Close()
			return err
		}
		if t.seatID == nil {
			standing = append(standing, t)
		} else {
			seated = append(seated, t)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Assento marcado: um job por ingresso.
	for _, t := range seated {
		if err := enqueueMaterialize(ctx, tx, t.id, 1, reason); err != nil {
			return err
		}
	}
	// Pista: um job por lote (representante + amount = tamanho do grupo).
	byLot := map[uuid.UUID][]tk{}
	for _, t := range standing {
		byLot[t.lotID] = append(byLot[t.lotID], t)
	}
	for _, group := range byLot {
		if err := enqueueMaterialize(ctx, tx, group[0].id, len(group), reason); err != nil {
			return err
		}
		for _, t := range group[1:] {
			if _, err := tx.Exec(ctx, `UPDATE tickets SET chain_status='pending', updated_at=now() WHERE id=$1`, t.id); err != nil {
				return err
			}
		}
	}
	return nil
}

// enqueueMaterialize cria o job de mint e marca o ingresso 'pending'.
func enqueueMaterialize(ctx context.Context, tx pgx.Tx, ticketID uuid.UUID, amount int, reason string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO chain_jobs (ticket_id, kind, reason, amount)
		VALUES ($1,'mint',$2,$3)`, ticketID, nilReason(reason), amount); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE tickets SET chain_status='pending', updated_at=now() WHERE id=$1`, ticketID)
	return err
}

func nilReason(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// BackfillFilter delimita o backfill (comando administrativo).
type BackfillFilter struct {
	EventID *uuid.UUID // nil = todos os eventos do produtor
	Since   *time.Time // nil = desde sempre
	Limit   int        // teto de ingressos por execução (<=0 = DefaultBackfillLimit)
}

// BackfillMaterialize materializa em lote o histórico não materializado de um produtor,
// respeitando o limite de taxa (teto de ingressos por execução). O teto de gás por execução
// é um parâmetro PROVISÓRIO: o gás só é conhecido após o mint (gas_cost_wei), então não há
// estimativa pré-mint aqui — o limite de taxa (ingressos) é o freio.
func BackfillMaterialize(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, f BackfillFilter) (int, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultBackfillLimit
	}
	var count int
	err := inTenant(ctx, pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id FROM tickets
			 WHERE chain_status = 'not_materialized'
			   AND ($1::uuid IS NULL OR event_id = $1)
			   AND ($2::timestamptz IS NULL OR created_at >= $2)
			 ORDER BY created_at
			 LIMIT $3
			 FOR UPDATE SKIP LOCKED`, f.EventID, f.Since, limit)
		if err != nil {
			return err
		}
		var ids []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		if err := Materialize(ctx, tx, ids, ReasonBackfill); err != nil {
			return err
		}
		count = len(ids)
		return nil
	})
	return count, err
}

// inTenant roda fn numa transação escopada ao tenant (commita no fim).
func inTenant(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenancy.WithTenant(ctx, tx, tenantID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
