package attest

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// LegalHalfPricePct é a cota de meia-entrada da Lei 12.933/2013, art. 1º — 40% do total de
// ingressos disponíveis para cada evento. É o DEFAULT do sistema e a referência do aviso,
// NÃO uma trava.
//
// A obrigação legal é do PRODUTOR. Recusar a configuração dele não o faz cumprir a lei: só o
// impede de operar e nos coloca no lugar de fiscal. O que cabe ao sistema é mostrar a regra,
// avisar quando a escolha fica abaixo dela e registrar quem escolheu — o que está em
// audit_events, com valor, data e usuário.
//
// A cota vale mesmo sem compromisso declarado: sem declaração nenhuma, vale a lei.
const LegalHalfPricePct = 40.0

// ErrHalfPriceSoldOut é a cota de meia esgotada. A inteira continua à venda: acabou a
// meia, não o evento.
var ErrHalfPriceSoldOut = errors.New("attest: cota de meia-entrada esgotada")

// HalfPriceAllowance é o estado da cota de meia de um evento.
type HalfPriceAllowance struct {
	// Capacity é a base da cota: a soma das quantidades dos lotes (o total de ingressos
	// disponíveis do evento, que é o que a lei mede).
	Capacity  int `json:"capacity"`
	Quota     int `json:"quota"`
	Granted   int `json:"granted"`
	Remaining int `json:"remaining"`
	// Declared diz se o produtor declarou uma cota própria.
	Declared bool `json:"declared"`
	// BelowLegal marca a cota declarada ABAIXO dos 40% da lei. Não impede nada — existe para
	// a tela dizer, e continuar dizendo, que a responsabilidade pelo cumprimento é do
	// produtor.
	BelowLegal bool `json:"below_legal"`
	// LegalQuota é quanto a lei pediria neste evento. Fica ao lado do escolhido para o aviso
	// ter número, e não só adjetivo.
	LegalQuota int `json:"legal_quota"`
	// Mode: 'quota' (cota própria) ou 'linked' (a meia segue o estoque do lote pai, sem
	// limite próprio).
	Mode string `json:"mode"`
}

// Modos da meia-entrada no evento.
const (
	// ModeQuota: a meia tem cota própria (declarada ou a legal).
	ModeQuota = "quota"
	// ModeLinked: a meia é vinculada à inteira e segue o estoque do lote pai, sem limite.
	ModeLinked = "linked"
)

// Available diz se ainda cabe meia-entrada.
func (a HalfPriceAllowance) Available() bool { return a.Remaining > 0 }

// HalfPrice calcula a cota aplicável, quanto já foi concedido e quanto resta.
//
// A cota declarada pelo produtor VALE, inclusive abaixo da legal — a escolha é dele e a
// responsabilidade também. O que o cálculo faz é marcar `BelowLegal` para a tela avisar. E o
// compromisso declarado vale na venda: uma cota que não barra nada é promessa sem
// consequência.
//
// No modo 'linked' não há cota: a meia segue o estoque do lote pai. Continua consumindo esse
// estoque — o que fica fora é estoque próprio que SOMA, que é o que faz a casa ser vendida
// duas vezes.
func HalfPrice(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (HalfPriceAllowance, error) {
	var a HalfPriceAllowance
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(quantity),0) FROM lots WHERE event_id=$1`, eventID).Scan(&a.Capacity); err != nil {
		return a, err
	}
	if err := tx.QueryRow(ctx, `SELECT half_price_mode FROM events WHERE id=$1`, eventID).Scan(&a.Mode); err != nil {
		return a, err
	}
	a.LegalQuota = int(math.Round(float64(a.Capacity) * LegalHalfPricePct / 100))
	a.Quota = a.LegalQuota

	var targetType, targetValue string
	err := tx.QueryRow(ctx, `
		SELECT target_type, target_value::text FROM event_commitments
		 WHERE event_id=$1 AND kind=$2 ORDER BY created_at LIMIT 1`, eventID, KindMeiaEntradaCota).
		Scan(&targetType, &targetValue)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Sem compromisso declarado: vale a lei.
	case err != nil:
		return a, err
	default:
		a.Declared = true
		if v, perr := parseValue(targetValue); perr == nil {
			declared := int(v)
			if targetType == TargetPercent {
				declared = int(math.Round(float64(a.Capacity) * v / 100))
			}
			a.Quota = declared
			a.BelowLegal = declared < a.LegalQuota
		}
	}

	// Vinculada: a meia não tem limite próprio. A cota publicada passa a ser a capacidade —
	// é a verdade ("cabe até tudo"), e mantém a forma do registro canônico intacta.
	if a.Mode == ModeLinked {
		a.Quota = a.Capacity
		a.BelowLegal = false
	}

	// Concedido é contado em INGRESSOS emitidos, não em compras: é assim que a lei mede, e
	// é o que faz um combo de duas meias consumir dois da cota, não um.
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM tickets
		 WHERE event_id=$1 AND half_price AND status IN ('active','used')`, eventID).Scan(&a.Granted); err != nil {
		return a, err
	}
	a.Remaining = max(a.Quota-a.Granted, 0)
	return a, nil
}

// EnsureHalfPrice recusa a venda que estouraria a cota. `qty` é quantas meias a compra
// pede. Chamado no funil de criação da ordem — a UI esconder o botão não basta.
func EnsureHalfPrice(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, qty int) error {
	if qty <= 0 {
		return nil
	}
	a, err := HalfPrice(ctx, tx, eventID)
	if err != nil {
		return err
	}
	if qty > a.Remaining {
		return ErrHalfPriceSoldOut
	}
	return nil
}

// SetHalfPrice grava a configuração de meia-entrada do evento: o modo e, quando há cota, o
// valor declarado. Uma chamada só, porque os dois são a MESMA decisão — trocar o modo sem
// mexer na cota deixaria uma cota órfã valendo depois.
//
// Cota abaixo dos 40% da lei é aceita. Quem chama avisa na tela e registra a escolha na
// trilha: a obrigação é do produtor, e um sistema que recusa a configuração dele não o faz
// cumprir a lei — só o impede de operar.
func SetHalfPrice(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, mode, targetType, targetValue string) (HalfPriceAllowance, error) {
	if mode != ModeQuota && mode != ModeLinked {
		return HalfPriceAllowance{}, fmt.Errorf("attest: modo de meia-entrada desconhecido: %q", mode)
	}
	if err := ensureOpen(ctx, tx, eventID); err != nil {
		return HalfPriceAllowance{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE events SET half_price_mode=$2, updated_at=now() WHERE id=$1`, eventID, mode); err != nil {
		return HalfPriceAllowance{}, err
	}
	// A cota anterior sai sempre: no modo vinculado ela não existe, e no modo cota o valor
	// novo substitui o antigo — dois compromissos de meia no mesmo evento fariam a leitura
	// depender da ordem de criação.
	if _, err := tx.Exec(ctx, `
		DELETE FROM event_commitments WHERE event_id=$1 AND kind=$2`, eventID, KindMeiaEntradaCota); err != nil {
		return HalfPriceAllowance{}, err
	}
	if mode == ModeQuota && targetValue != "" {
		if targetType != TargetPercent && targetType != TargetAbsolute {
			return HalfPriceAllowance{}, fmt.Errorf("attest: target_type inválido: %q", targetType)
		}
		if _, err := CreateCommitment(ctx, tx, Commitment{
			EventID: eventID, Kind: KindMeiaEntradaCota,
			TargetType: targetType, TargetValue: targetValue,
		}); err != nil {
			return HalfPriceAllowance{}, err
		}
	}
	return HalfPrice(ctx, tx, eventID)
}
