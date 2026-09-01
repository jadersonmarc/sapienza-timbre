package catalog

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrLinkInvalid é o link que não abre: desconhecido, revogado, vencido ou com os usos
// esgotados. Os quatro casos dão a MESMA resposta de propósito — distinguir "não existe" de
// "foi revogado" para quem só tem o token é entregar informação a quem está tentando.
var ErrLinkInvalid = errors.New("catalog: link inválido, revogado, vencido ou esgotado")

// LotLink é o acesso exclusivo a uma categoria oculta.
type LotLink struct {
	ID    uuid.UUID `json:"id"`
	LotID uuid.UUID `json:"lot_id"`
	// Token viaja na URL. É gerado por crypto/rand e guardado inteiro: não é sequencial,
	// não é derivado do id do lote e não dá para chegar nele a partir de outro.
	Token   string     `json:"token"`
	Label   *string    `json:"label,omitempty"`
	MaxUses *int       `json:"max_uses,omitempty"` // nulo = sem limite
	Used    int        `json:"used_count"`
	Expires *time.Time `json:"expires_at,omitempty"`
	Revoked *time.Time `json:"revoked_at,omitempty"`
	// Active resume o estado para a tela: um link vencido e um revogado parecem iguais para
	// quem tenta usar, mas são coisas diferentes para quem administra.
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}

const linkCols = `id, lot_id, token, label, max_uses, used_count, expires_at, revoked_at, created_at`

func scanLink(row pgx.Row) (LotLink, error) {
	var l LotLink
	err := row.Scan(&l.ID, &l.LotID, &l.Token, &l.Label, &l.MaxUses, &l.Used,
		&l.Expires, &l.Revoked, &l.CreatedAt)
	l.Active = l.usable(time.Now())
	return l, err
}

// usable diz se o link ainda abre.
func (l LotLink) usable(now time.Time) bool {
	if l.Revoked != nil {
		return false
	}
	if l.Expires != nil && !l.Expires.After(now) {
		return false
	}
	if l.MaxUses != nil && l.Used >= *l.MaxUses {
		return false
	}
	return true
}

// newLinkToken gera o token do link. 32 bytes de crypto/rand em base64 url-safe: não
// sequencial, não adivinhável e curto o bastante para caber num link compartilhável.
func newLinkToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("gerar token do link: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// CreateLotLink cria um link exclusivo para a categoria. Criar o link marca a categoria como
// OCULTA: link privado para algo que já aparece na página não é privado.
func CreateLotLink(ctx context.Context, tx pgx.Tx, lotID uuid.UUID, label string, maxUses *int, expires *time.Time) (LotLink, error) {
	if maxUses != nil && *maxUses <= 0 {
		return LotLink{}, fmt.Errorf("catalog: o limite de usos precisa ser maior que zero")
	}
	token, err := newLinkToken()
	if err != nil {
		return LotLink{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE lots SET hidden = true, updated_at = now() WHERE id = $1`, lotID); err != nil {
		return LotLink{}, err
	}
	return scanLink(tx.QueryRow(ctx, `
		INSERT INTO lot_links (lot_id, token, label, max_uses, expires_at)
		VALUES ($1,$2,$3,$4,$5) RETURNING `+linkCols,
		lotID, token, nilIfEmpty(label), maxUses, expires))
}

// ListLotLinks lista os links das categorias de um evento.
func ListLotLinks(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) ([]LotLink, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+linkCols+` FROM lot_links
		 WHERE lot_id IN (SELECT id FROM lots WHERE event_id = $1)
		 ORDER BY created_at DESC`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LotLink{}
	for rows.Next() {
		var l LotLink
		if err := rows.Scan(&l.ID, &l.LotID, &l.Token, &l.Label, &l.MaxUses, &l.Used,
			&l.Expires, &l.Revoked, &l.CreatedAt); err != nil {
			return nil, err
		}
		l.Active = l.usable(time.Now())
		out = append(out, l)
	}
	return out, rows.Err()
}

// RevokeLotLink desliga o link. A checagem acontece a CADA uso, então ele para de funcionar
// na hora — não na próxima virada de cache.
func RevokeLotLink(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		UPDATE lot_links SET revoked_at = COALESCE(revoked_at, now()) WHERE id = $1`, id)
	return err
}

// ResolveLotLink devolve a categoria que o token abre, se ele ainda for válido. O evento
// entra na consulta para um token de um evento não abrir categoria de outro.
func ResolveLotLink(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, token string) (Lot, LotLink, error) {
	if token == "" {
		return Lot{}, LotLink{}, ErrLinkInvalid
	}
	link, err := scanLink(tx.QueryRow(ctx, `
		SELECT `+linkCols+` FROM lot_links l
		 WHERE l.token = $1 AND EXISTS (SELECT 1 FROM lots WHERE id = l.lot_id AND event_id = $2)`,
		token, eventID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Lot{}, LotLink{}, ErrLinkInvalid
	}
	if err != nil {
		return Lot{}, LotLink{}, err
	}
	if !link.usable(time.Now()) {
		return Lot{}, LotLink{}, ErrLinkInvalid
	}
	lot, err := GetLot(ctx, tx, link.LotID)
	if err != nil {
		return Lot{}, LotLink{}, err
	}
	return lot, link, nil
}

// ConsumeLotLink conta o uso do link. É chamado na CONFIRMAÇÃO do pagamento, não na criação
// da sessão: um Pix aberto e abandonado não pode gastar a vaga de ninguém.
//
// O UPDATE condicional é quem garante o limite — duas confirmações simultâneas não passam
// do teto porque a segunda não afeta linha nenhuma.
func ConsumeLotLink(ctx context.Context, tx pgx.Tx, id uuid.UUID, qty int) error {
	if qty <= 0 {
		return nil
	}
	tag, err := tx.Exec(ctx, `
		UPDATE lot_links SET used_count = used_count + $2
		 WHERE id = $1 AND revoked_at IS NULL
		   AND (max_uses IS NULL OR used_count + $2 <= max_uses)`, id, qty)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLinkInvalid
	}
	return nil
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
