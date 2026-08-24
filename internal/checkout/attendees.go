package checkout

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var nonDigits = regexp.MustCompile(`\D`)

// NormalizeAttendees limpa e valida a ficha nominal. Devolve a primeira pendência em texto
// que o comprador entenda. A ficha é opcional; informada, precisa estar completa — nome
// pela metade não serve para conferir documento na portaria.
func NormalizeAttendees(in []Attendee, quantity int) ([]Attendee, string) {
	if len(in) == 0 {
		return nil, ""
	}
	if len(in) != quantity {
		return nil, fmt.Sprintf("informe os dados dos %d participantes", quantity)
	}
	out := make([]Attendee, 0, len(in))
	seen := make(map[string]bool, len(in))
	for i, a := range in {
		name := strings.Join(strings.Fields(a.Name), " ")
		if len(strings.Fields(name)) < 2 {
			return nil, fmt.Sprintf("informe o nome completo do participante %d", i+1)
		}
		cpf := nonDigits.ReplaceAllString(a.CPF, "")
		if !ValidCPF(cpf) {
			return nil, fmt.Sprintf("CPF inválido no participante %d", i+1)
		}
		// Mesmo CPF em dois ingressos do pedido: uma pessoa não ocupa dois lugares, e
		// deixar passar transforma a meia-entrada num vale para revenda.
		if seen[cpf] {
			return nil, "há CPF repetido entre os participantes"
		}
		seen[cpf] = true
		out = append(out, Attendee{Name: name, CPF: cpf, Email: strings.ToLower(strings.TrimSpace(a.Email))})
	}
	return out, ""
}

// ValidCPF confere os dígitos verificadores (e recusa os repetidos, que passam no cálculo).
func ValidCPF(cpf string) bool {
	if len(cpf) != 11 {
		return false
	}
	same := true
	for i := 1; i < 11; i++ {
		if cpf[i] != cpf[0] {
			same = false
			break
		}
	}
	if same {
		return false
	}
	for _, pos := range []int{9, 10} {
		sum := 0
		for i := 0; i < pos; i++ {
			sum += int(cpf[i]-'0') * (pos + 1 - i)
		}
		d := (sum * 10) % 11
		if d == 10 {
			d = 0
		}
		if d != int(cpf[pos]-'0') {
			return false
		}
	}
	return true
}

// attendeesJSON serializa a ficha para a coluna jsonb da ordem.
func attendeesJSON(a []Attendee) []byte {
	if len(a) == 0 {
		return []byte(`[]`)
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return []byte(`[]`)
	}
	return raw
}

// applyAttendees nomeia os ingressos da ordem com a ficha registrada, na ordem de emissão.
// Ordem sem ficha não faz nada — a compra continua válida.
func applyAttendees(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, tickets []uuid.UUID) error {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT attendees FROM orders WHERE id=$1`, orderID).Scan(&raw); err != nil {
		return err
	}
	var list []Attendee
	if err := json.Unmarshal(raw, &list); err != nil || len(list) == 0 {
		return nil
	}
	for i, tid := range tickets {
		if i >= len(list) {
			break
		}
		if _, err := tx.Exec(ctx, `
			UPDATE tickets SET attendee_name=$2, attendee_cpf=$3, attendee_email=$4, updated_at=now()
			 WHERE id=$1`, tid, list[i].Name, list[i].CPF, nilIfEmpty(list[i].Email)); err != nil {
			return fmt.Errorf("nomear ingresso: %w", err)
		}
	}
	return nil
}
