// Package inventory é o motor de reserva de assentos (Etapa 1.3) — a peça mais
// delicada do sistema. Todas as funções recebem uma pgx.Tx já escopada por
// tenancy.WithTenant.
//
// Garantia de exclusividade: NÃO vem da aplicação, e sim do índice único parcial
// `seat_occupancy_live_key` (event_id, seat_id) WHERE NOT released. Hold e a emissão
// de ingresso escrevem em seat_occupancy; o índice garante que um assento tenha no
// máximo UMA ocupação viva. É isso que faz N compradores disputando o mesmo assento
// terem exatamente um vencedor.
package inventory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DefaultTTL é o tempo de vida padrão de um hold.
const DefaultTTL = 10 * time.Minute

// AbandonedOrderTTL é o prazo após o qual uma ordem 'pending' (paga nunca confirmada) é
// tratada como abandonada e a reserva de LOTE (held_count) é devolvida. Constante
// isolada — valor PROVISÓRIO: sem definição de negócio, adotado maior que DefaultTTL do
// hold para não competir com uma compra em andamento. Reportar para calibrar.
const AbandonedOrderTTL = 30 * time.Minute

var (
	// ErrSeatUnavailable: algum assento já está ocupado (hold vivo ou ingresso).
	ErrSeatUnavailable = errors.New("inventory: um ou mais assentos indisponíveis")
	// ErrSeatInvalid: assento inexistente ou de outro evento.
	ErrSeatInvalid = errors.New("inventory: assento inexistente ou de outro evento")
	// ErrSeatBlocked: assento bloqueado (poltrona quebrada, técnica, imprensa...).
	ErrSeatBlocked = errors.New("inventory: assento bloqueado")
	// ErrHoldNotHeld: hold inexistente ou não está mais ativo.
	ErrHoldNotHeld = errors.New("inventory: hold inexistente ou não está ativo")
	// ErrHoldExpired: hold expirou antes do Confirm.
	ErrHoldExpired = errors.New("inventory: hold expirado")
	// ErrAntiHole: a reserva deixaria um assento isolado (regra anti-buraco).
	ErrAntiHole = errors.New("inventory: reserva deixaria assento isolado (anti-buraco)")
	// ErrNoSeats: nenhum assento informado.
	ErrNoSeats = errors.New("inventory: informe ao menos um assento")
)

func seatStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// Hold reserva N assentos de forma ATÔMICA (todos ou nenhum) por `ttl`, e devolve o
// id da reserva (o grupo). Se qualquer assento já estiver ocupado, falha inteira com
// ErrSeatUnavailable. Trava as linhas dos assentos (FOR UPDATE) para serializar
// disputas e limpa holds vencidos oportunisticamente antes de tentar.
func Hold(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, seatIDs []uuid.UUID, ttl time.Duration) (uuid.UUID, error) {
	if len(seatIDs) == 0 {
		return uuid.Nil, ErrNoSeats
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	ids := seatStrings(seatIDs)

	// 1. Trava as linhas dos assentos e valida (existem, do evento, não bloqueados).
	rows, err := tx.Query(ctx, `
		SELECT s.id, s.blocked_reason, se.event_id
		  FROM seats s JOIN sectors se ON se.id = s.sector_id
		 WHERE s.id = ANY($1::uuid[])
		 FOR UPDATE OF s`, ids)
	if err != nil {
		return uuid.Nil, fmt.Errorf("travar assentos: %w", err)
	}
	found := 0
	for rows.Next() {
		var id uuid.UUID
		var blocked *string
		var evID uuid.UUID
		if err := rows.Scan(&id, &blocked, &evID); err != nil {
			rows.Close()
			return uuid.Nil, err
		}
		found++
		if evID != eventID {
			rows.Close()
			return uuid.Nil, ErrSeatInvalid
		}
		if blocked != nil {
			rows.Close()
			return uuid.Nil, ErrSeatBlocked
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return uuid.Nil, err
	}
	if found != len(seatIDs) {
		return uuid.Nil, ErrSeatInvalid
	}

	// 2. Expira holds vencidos nesses assentos (não bloqueiam um novo comprador).
	if _, err := tx.Exec(ctx, `
		UPDATE seat_occupancy SET released = true
		 WHERE seat_id = ANY($1::uuid[]) AND kind = 'hold' AND NOT released AND expires_at <= now()`, ids); err != nil {
		return uuid.Nil, fmt.Errorf("expirar holds vencidos: %w", err)
	}

	// 3. Regra anti-buraco (opcional por evento).
	if err := checkAntiHole(ctx, tx, eventID, seatIDs); err != nil {
		return uuid.Nil, err
	}

	// 4. Cria o grupo (holds) e a ocupação por assento. O índice único parcial faz a
	//    reserva falhar inteira se qualquer assento já estiver ocupado.
	var holdID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO holds (event_id, status, expires_at)
		VALUES ($1, 'held', now() + $2::interval)
		RETURNING id`, eventID, fmt.Sprintf("%d seconds", int(ttl.Seconds()))).Scan(&holdID); err != nil {
		return uuid.Nil, fmt.Errorf("criar hold: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO seat_occupancy (event_id, seat_id, kind, hold_id, expires_at)
		SELECT $1, s, 'hold', $2, now() + $3::interval
		  FROM unnest($4::uuid[]) AS s`,
		eventID, holdID, fmt.Sprintf("%d seconds", int(ttl.Seconds())), ids); err != nil {
		if isUniqueViolation(err) {
			return uuid.Nil, ErrSeatUnavailable
		}
		return uuid.Nil, fmt.Errorf("ocupar assentos: %w", err)
	}
	return holdID, nil
}

// Release libera um hold (idempotente): marca a ocupação como released e o grupo como
// 'released'. Assentos voltam a ficar disponíveis.
func Release(ctx context.Context, tx pgx.Tx, holdID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `
		UPDATE seat_occupancy SET released = true
		 WHERE hold_id = $1 AND kind = 'hold' AND NOT released`, holdID); err != nil {
		return fmt.Errorf("liberar ocupação: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE holds SET status = 'released' WHERE id = $1 AND status = 'held'`, holdID); err != nil {
		return fmt.Errorf("liberar hold: %w", err)
	}
	return nil
}

// Confirm converte um hold em ingressos NA MESMA TRANSAÇÃO: emite um ticket por
// assento e transforma a ocupação de 'hold' em 'ticket' (o assento nunca fica livre
// entre o hold e o ingresso). transferableAfter é calculado pelo checkout (imediato
// em Pix, 60 dias em cartão). Assinatura Ed25519/QR ficam para a Etapa 1.5.
func Confirm(ctx context.Context, tx pgx.Tx, holdID, orderID, lotID uuid.UUID, transferableAfter time.Time) ([]uuid.UUID, error) {
	// Trava e valida o grupo.
	var eventID uuid.UUID
	var status string
	var live bool
	if err := tx.QueryRow(ctx, `
		SELECT event_id, status, expires_at > now()
		  FROM holds WHERE id = $1 FOR UPDATE`, holdID).Scan(&eventID, &status, &live); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrHoldNotHeld
		}
		return nil, err
	}
	if status != "held" {
		return nil, ErrHoldNotHeld
	}
	if !live {
		return nil, ErrHoldExpired
	}

	occRows, err := tx.Query(ctx, `
		SELECT id, seat_id FROM seat_occupancy
		 WHERE hold_id = $1 AND kind = 'hold' AND NOT released`, holdID)
	if err != nil {
		return nil, err
	}
	type occ struct {
		id, seatID uuid.UUID
	}
	var occs []occ
	for occRows.Next() {
		var o occ
		if err := occRows.Scan(&o.id, &o.seatID); err != nil {
			occRows.Close()
			return nil, err
		}
		occs = append(occs, o)
	}
	occRows.Close()
	if err := occRows.Err(); err != nil {
		return nil, err
	}

	ticketIDs := make([]uuid.UUID, 0, len(occs))
	for _, o := range occs {
		var ticketID uuid.UUID
		if err := tx.QueryRow(ctx, `
			INSERT INTO tickets (event_id, lot_id, order_id, seat_id, transferable_after, status, chain_status)
			VALUES ($1, $2, $3, $4, $5, 'active', 'not_materialized')
			RETURNING id`, eventID, lotID, orderID, o.seatID, transferableAfter).Scan(&ticketID); err != nil {
			return nil, fmt.Errorf("emitir ingresso: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE seat_occupancy SET kind = 'ticket', ticket_id = $2, expires_at = NULL
			 WHERE id = $1`, o.id, ticketID); err != nil {
			return nil, fmt.Errorf("converter ocupação em ingresso: %w", err)
		}
		ticketIDs = append(ticketIDs, ticketID)
	}
	if _, err := tx.Exec(ctx, `UPDATE holds SET status = 'confirmed', order_id = $2 WHERE id = $1`, holdID, orderID); err != nil {
		return nil, fmt.Errorf("confirmar hold: %w", err)
	}
	return ticketIDs, nil
}

// ExpireDue libera holds vencidos no schema atual (uma varredura). Usa FOR UPDATE
// SKIP LOCKED para não brigar com Holds/Confirms em andamento. Devolve quantas
// ocupações liberou.
func ExpireDue(ctx context.Context, tx pgx.Tx) (int64, error) {
	tag, err := tx.Exec(ctx, `
		WITH due AS (
			SELECT id FROM seat_occupancy
			 WHERE kind = 'hold' AND NOT released AND expires_at <= now()
			 FOR UPDATE SKIP LOCKED
		)
		UPDATE seat_occupancy o SET released = true
		  FROM due WHERE o.id = due.id`)
	if err != nil {
		return 0, fmt.Errorf("expirar ocupações: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE holds SET status = 'expired' WHERE status = 'held' AND expires_at <= now()`); err != nil {
		return 0, fmt.Errorf("expirar holds: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ReleaseAbandonedLotHolds devolve a reserva de LOTE (held_count) de ordens 'pending'
// além de AbandonedOrderTTL — a fuga de held quando o pagamento nunca confirma. Marca a
// ordem como 'cancelled' para não liberar duas vezes. O held de assento já é expirado
// por ExpireDue (por expires_at do seat_occupancy). Decremento com piso em 0. Devolve
// quantas ordens cancelou.
func ReleaseAbandonedLotHolds(ctx context.Context, tx pgx.Tx, ttl time.Duration) (int64, error) {
	interval := fmt.Sprintf("%d seconds", int(ttl.Seconds()))
	tag, err := tx.Exec(ctx, `
		WITH stale AS (
			SELECT id FROM orders
			 WHERE status = 'pending' AND created_at <= now() - $1::interval
			 FOR UPDATE SKIP LOCKED
		),
		agg AS (
			SELECT oi.lot_id, SUM(oi.quantity)::int AS qty
			  FROM order_items oi JOIN stale ON stale.id = oi.order_id
			 GROUP BY oi.lot_id
		),
		rel AS (
			UPDATE lots l SET held_count = GREATEST(l.held_count - agg.qty, 0), updated_at = now()
			  FROM agg WHERE l.id = agg.lot_id
			RETURNING 1
		)
		UPDATE orders o SET status = 'cancelled', updated_at = now()
		  FROM stale WHERE o.id = stale.id`, interval)
	if err != nil {
		return 0, fmt.Errorf("liberar held de ordens abandonadas: %w", err)
	}
	return tag.RowsAffected(), nil
}

// checkAntiHole aplica a regra anti-buraco quando o evento a tem ligada: em cada
// fileira afetada, a reserva não pode deixar um assento livre isolado entre dois
// ocupados. Considera como ocupados os assentos já com ocupação viva mais os que
// estão sendo reservados agora. Assentos numéricos por fileira (o gerador de grade
// produz números); ordena por número.
func checkAntiHole(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, seatIDs []uuid.UUID) error {
	var antiHole bool
	if err := tx.QueryRow(ctx, `SELECT anti_hole FROM events WHERE id = $1`, eventID).Scan(&antiHole); err != nil {
		return err
	}
	if !antiHole {
		return nil
	}
	ids := seatStrings(seatIDs)

	// Fileiras (sector_id, row_label) tocadas pela reserva.
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT sector_id, COALESCE(row_label, '')
		  FROM seats WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return err
	}
	type rowKey struct {
		sector uuid.UUID
		label  string
	}
	var keys []rowKey
	for rows.Next() {
		var k rowKey
		if err := rows.Scan(&k.sector, &k.label); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	held := make(map[uuid.UUID]bool, len(seatIDs))
	for _, id := range seatIDs {
		held[id] = true
	}

	for _, k := range keys {
		// Assentos da fileira, em ordem (número como inteiro quando possível).
		sRows, err := tx.Query(ctx, `
			SELECT s.id,
			       EXISTS (SELECT 1 FROM seat_occupancy o
			                WHERE o.seat_id = s.id AND NOT o.released) AS occupied
			  FROM seats s
			 WHERE s.sector_id = $1 AND COALESCE(s.row_label,'') = $2
			 ORDER BY NULLIF(regexp_replace(s.number, '\D', '', 'g'), '')::int NULLS LAST, s.number`,
			k.sector, k.label)
		if err != nil {
			return err
		}
		type seatState struct {
			id       uuid.UUID
			occupied bool
		}
		var line []seatState
		for sRows.Next() {
			var st seatState
			if err := sRows.Scan(&st.id, &st.occupied); err != nil {
				sRows.Close()
				return err
			}
			// A reserva atual conta como ocupada na verificação.
			if held[st.id] {
				st.occupied = true
			}
			line = append(line, st)
		}
		sRows.Close()
		if err := sRows.Err(); err != nil {
			return err
		}
		// Um buraco = assento livre com vizinhos ocupados dos dois lados.
		for i := 1; i < len(line)-1; i++ {
			if !line[i].occupied && line[i-1].occupied && line[i+1].occupied {
				return ErrAntiHole
			}
		}
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
