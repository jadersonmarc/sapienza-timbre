// Package attest implementa o fechamento do evento e a atestação: categorias de cortesia,
// compromissos declarados (contrapartida), o registro canônico determinístico assinado e a
// âncora opcional. O eixo on-chain aqui é PROVA, não posse.
package attest

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ── categorias de cortesia ───────────────────────────────────────────────────

// Category é uma linha de courtesy_categories (por tenant).
type Category struct {
	ID        uuid.UUID `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	SortOrder int       `json:"sort_order"`
	Active    bool      `json:"active"`
}

// ListCategories lista as categorias (ativas por padrão; all=true inclui inativas).
func ListCategories(ctx context.Context, tx pgx.Tx, all bool) ([]Category, error) {
	q := `SELECT id, slug, name, sort_order, active FROM courtesy_categories`
	if !all {
		q += ` WHERE active`
	}
	q += ` ORDER BY sort_order, name`
	rows, err := tx.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Slug, &c.Name, &c.SortOrder, &c.Active); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateCategory cria uma categoria de cortesia.
func CreateCategory(ctx context.Context, tx pgx.Tx, slug, name string, sortOrder int) (Category, error) {
	var c Category
	err := tx.QueryRow(ctx, `
		INSERT INTO courtesy_categories (slug, name, sort_order) VALUES ($1,$2,$3)
		RETURNING id, slug, name, sort_order, active`, slug, name, sortOrder).
		Scan(&c.ID, &c.Slug, &c.Name, &c.SortOrder, &c.Active)
	return c, err
}

// UpdateCategory atualiza nome/ordem/ativo de uma categoria.
//
// Os três campos são opcionais e o nil preserva o que está gravado: arquivar uma categoria
// manda só `active`, e o nome dela não pode ir junto para vazio no caminho.
func UpdateCategory(ctx context.Context, tx pgx.Tx, id uuid.UUID, name *string, sortOrder *int, active *bool) error {
	_, err := tx.Exec(ctx, `
		UPDATE courtesy_categories
		   SET name       = COALESCE($2, name),
		       sort_order = COALESCE($3, sort_order),
		       active     = COALESCE($4, active)
		 WHERE id = $1`,
		id, name, sortOrder, active)
	return err
}

// ── compromissos declarados ──────────────────────────────────────────────────

// Kinds de compromisso.
const (
	KindCourtesyShare   = "courtesy_share"
	KindMeiaEntradaCota = "meia_entrada_cota"
	KindFreeAdmission   = "free_admission"
	KindCustom          = "custom"
)

// TargetTypes de compromisso.
const (
	TargetPercent  = "percent"
	TargetAbsolute = "absolute"
)

// ErrCommitmentClosed: o evento já foi fechado — compromissos/cortesias/lotes travados.
var ErrClosed = errors.New("attest: evento já fechado")

// ErrCommitmentOverflow: a soma dos compromissos percentuais excede 100%.
var ErrCommitmentOverflow = errors.New("attest: soma dos compromissos percentuais excede 100%")

// ErrCommitmentMissingCategory: courtesy_share exige categoria.
var ErrCommitmentMissingCategory = errors.New("attest: courtesy_share exige categoria")

// Commitment é um compromisso declarado pelo organizador.
type Commitment struct {
	ID                 uuid.UUID  `json:"id"`
	EventID            uuid.UUID  `json:"event_id"`
	Kind               string     `json:"kind"`
	CourtesyCategoryID *uuid.UUID `json:"courtesy_category_id,omitempty"`
	TargetType         string     `json:"target_type"`
	TargetValue        string     `json:"target_value"` // decimal canônico (string)
	Description        *string    `json:"description,omitempty"`
}

// CreateCommitment valida e cria um compromisso. Bloqueado após o fechamento do evento.
func CreateCommitment(ctx context.Context, tx pgx.Tx, c Commitment) (Commitment, error) {
	if err := ensureOpen(ctx, tx, c.EventID); err != nil {
		return Commitment{}, err
	}
	if c.Kind == KindCourtesyShare && c.CourtesyCategoryID == nil {
		return Commitment{}, ErrCommitmentMissingCategory
	}
	val, err := parseValue(c.TargetValue)
	if err != nil {
		return Commitment{}, fmt.Errorf("target_value inválido: %w", err)
	}
	// Cota de meia abaixo dos 40% da lei é ACEITA. A obrigação é do produtor, e recusar a
	// configuração dele não o faz cumprir a lei — só o impede de operar. Quem chama avisa na
	// tela e grava a escolha na trilha; ver internal/attest/halfprice.go.
	if c.TargetType == TargetPercent {
		if val < 0 || val > 100 {
			return Commitment{}, fmt.Errorf("percentual fora de 0..100")
		}
		// A soma dos percentuais (por tipo/kind) não pode passar de 100 no mesmo evento.
		var sum float64
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(target_value),0) FROM event_commitments
			 WHERE event_id=$1 AND kind=$2 AND target_type='percent'`,
			c.EventID, c.Kind).Scan(&sum); err != nil {
			return Commitment{}, err
		}
		if sum+val > 100 {
			return Commitment{}, ErrCommitmentOverflow
		}
	}
	var out Commitment
	out = c
	err = tx.QueryRow(ctx, `
		INSERT INTO event_commitments (event_id, kind, courtesy_category_id, target_type, target_value, description)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id, target_value`, c.EventID, c.Kind, c.CourtesyCategoryID, c.TargetType, c.TargetValue, c.Description).
		Scan(&out.ID, &out.TargetValue)
	return out, err
}

// ListCommitments lista os compromissos de um evento.
func ListCommitments(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) ([]Commitment, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, event_id, kind, courtesy_category_id, target_type, target_value::text, description
		  FROM event_commitments WHERE event_id=$1 ORDER BY created_at, id`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Commitment
	for rows.Next() {
		var c Commitment
		if err := rows.Scan(&c.ID, &c.EventID, &c.Kind, &c.CourtesyCategoryID, &c.TargetType, &c.TargetValue, &c.Description); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteCommitment remove um compromisso. Bloqueado após o fechamento.
func DeleteCommitment(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	var eventID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT event_id FROM event_commitments WHERE id=$1`, id).Scan(&eventID); err != nil {
		return err
	}
	if err := ensureOpen(ctx, tx, eventID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM event_commitments WHERE id=$1`, id)
	return err
}

// ensureOpen falha se o evento já tem atestado vigente (fechado).
func ensureOpen(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) error {
	var closed bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM event_attestations WHERE event_id=$1 AND supersedes_id IS NULL)`, eventID).Scan(&closed); err != nil {
		return err
	}
	if closed {
		return ErrClosed
	}
	return nil
}

func parseValue(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("vazio")
	}
	return strconv.ParseFloat(s, 64)
}
