package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/ledger"
	"github.com/jadersonmarc/sapienza-timbre/internal/store"
)

// payoutDue é uma linha da fila de repasse: quanto um produtor tem a receber e para onde
// mandar. Os dados bancários vêm INTEIROS aqui — quem opera precisa copiar a chave para
// fazer a transferência; mascarar seria só teatro num painel que já exige ser admin.
type payoutDue struct {
	ProducerID   uuid.UUID `json:"producer_id"`
	ProducerName string    `json:"producer_name"`
	// NetDueCents é o que já pode ser transferido; UpcomingCents é o que ainda está preso
	// pelo prazo (o repasse libera D+2 depois do evento). Sem separar os dois, a fila ou
	// esconde trabalho futuro ou manda transferir dinheiro que ainda não é do produtor.
	NetDueCents   int64      `json:"net_due_cents"`
	UpcomingCents int64      `json:"upcoming_cents"`
	PendingCents  int64      `json:"pending_cents"`
	PixKey        string     `json:"pix_key"`
	PixKeyType    string     `json:"pix_key_type"`
	HolderName    string     `json:"holder_name"`
	HolderTaxID   string     `json:"holder_tax_id"`
	OldestDue     *time.Time `json:"oldest_due"`
	// Blocked marca quem tem dinheiro a receber e nenhum destino cadastrado — é o caso
	// que precisa de cobrança ativa, não de transferência.
	Blocked bool `json:"blocked"`
}

// adminPayouts lista o que a plataforma deve a cada produtor. Enquanto a divisão automática
// na venda não estiver em uso, o dinheiro entra centralizado e a transferência é feita por
// fora — então esta é a lista de trabalho de quem paga, com o valor que o razão calculou.
func (s *Server) adminPayouts(w http.ResponseWriter, r *http.Request, _ *auth.AdminClaims) {
	ctx := r.Context()
	producers, err := store.ListProducers(ctx, s.pool, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	list := make([]payoutDue, 0, len(producers))
	var total int64
	for _, p := range producers {
		due := payoutDue{ProducerID: p.ID, ProducerName: p.Name}
		acc, err := store.GetPayoutAccount(ctx, s.pool, p.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		due.PixKey, due.PixKeyType = acc.PixKey, acc.PixKeyType
		due.HolderName, due.HolderTaxID = acc.HolderName, acc.HolderTaxID

		if err := s.withTenant(ctx, p.ID, func(tx pgx.Tx) error {
			net, e := ledger.NetDue(ctx, tx)
			if e != nil {
				return e
			}
			due.NetDueCents = net
			if e := tx.QueryRow(ctx, `
				SELECT COALESCE(SUM(amount_cents),0)
				  FROM ledger_entries
				 WHERE kind='repasse' AND available_at IS NOT NULL AND available_at > now()`).
				Scan(&due.UpcomingCents); e != nil {
				return e
			}
			return tx.QueryRow(ctx, `
				SELECT COALESCE(SUM(amount_cents),0), MIN(created_at)
				  FROM payouts WHERE status='pending'`).Scan(&due.PendingCents, &due.OldestDue)
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Produtor sem venda nenhuma não é trabalho de ninguém: fica fora da lista.
		if due.NetDueCents <= 0 && due.PendingCents <= 0 && due.UpcomingCents <= 0 {
			continue
		}
		// Um split configurado significa que o dinheiro já foi direto para ele na venda.
		if p.AsaasWalletID != nil && *p.AsaasWalletID != "" {
			continue
		}
		// Bloqueado é quem tem dinheiro (agora ou a liberar) e nenhum destino: cobrar o
		// cadastro dele ANTES do prazo evita a transferência atrasar por isso.
		due.Blocked = due.PixKey == ""
		total += due.PendingCents
		list = append(list, due)
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_pending_cents": total, "producers": list})
}

type markPaidReq struct {
	PayoutID  uuid.UUID `json:"payout_id"`
	Reference string    `json:"reference"` // identificador da transferência (e2e do Pix)
}

// adminMarkPayoutPaid registra que a transferência saiu. A referência é o comprovante: sem
// ela, "pago" vira palavra contra palavra na primeira divergência.
func (s *Server) adminMarkPayoutPaid(w http.ResponseWriter, r *http.Request, _ *auth.AdminClaims) {
	producerID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var body markPaidReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	if body.Reference == "" {
		writeErr(w, http.StatusBadRequest, "informe a referência da transferência")
		return
	}
	var updated int64
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		tag, e := tx.Exec(r.Context(), `
			UPDATE payouts SET status='sent', asaas_ref=$2, sent_at=now()
			 WHERE id=$1 AND status='pending'`, body.PayoutID, body.Reference)
		if e != nil {
			return e
		}
		updated = tag.RowsAffected()
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if updated == 0 {
		writeErr(w, http.StatusNotFound, "repasse não encontrado ou já pago")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
