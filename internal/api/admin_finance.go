package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/checkout"
	"github.com/jadersonmarc/sapienza-timbre/internal/store"
	"github.com/jadersonmarc/sapienza-timbre/internal/subaccount"
)

// adminSubaccounts lista a situação da conta de recebimento de cada produtor. É a tela que
// responde "por que fulano não consegue vender?" — o estado da análise, o link de
// documentação pendente e a data da confirmação anual, tudo num lugar só.
func (s *Server) adminSubaccounts(w http.ResponseWriter, r *http.Request, _ *auth.AdminClaims) {
	producers, err := store.ListProducers(r.Context(), s.pool, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type row struct {
		ProducerID              uuid.UUID  `json:"producer_id"`
		ProducerName            string     `json:"producer_name"`
		Status                  string     `json:"account_status"`
		WalletID                string     `json:"wallet_id,omitempty"`
		PersonType              string     `json:"person_type,omitempty"`
		OnboardingURL           string     `json:"onboarding_url,omitempty"`
		StatusReason            string     `json:"status_reason,omitempty"`
		CanSell                 bool       `json:"can_sell"`
		CommercialInfoExpiresAt *time.Time `json:"commercial_info_expires_at,omitempty"`
		// CommercialInfoDue avisa que a confirmação anual está próxima ou vencida: sem
		// ela a subconta perde o uso da API, e o produtor descobriria vendendo.
		CommercialInfoDue bool `json:"commercial_info_due"`
	}
	list := make([]row, 0, len(producers))
	var semConta, aguardando, aprovadas int
	for _, p := range producers {
		acc, err := s.seams.Subaccounts.Get(r.Context(), p.ID)
		if err != nil && err != subaccount.ErrNotFound {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		it := row{
			ProducerID: p.ID, ProducerName: p.Name,
			Status: statusOrNone(acc.Status), WalletID: acc.WalletID, PersonType: acc.PersonType,
			OnboardingURL: acc.OnboardingURL, StatusReason: acc.StatusReason,
			CanSell: acc.CanSell(), CommercialInfoExpiresAt: acc.CommercialInfoExpiresAt,
		}
		if acc.CommercialInfoExpiresAt != nil {
			it.CommercialInfoDue = time.Until(*acc.CommercialInfoExpiresAt) < 30*24*time.Hour
		}
		switch it.Status {
		case subaccount.StatusNone:
			semConta++
		case subaccount.StatusApproved:
			aprovadas++
		default:
			aguardando++
		}
		list = append(list, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"producers": list,
		"resumo": map[string]int{
			"sem_conta": semConta, "aguardando": aguardando, "aprovadas": aprovadas,
		},
		// Período de avaliação regulatória: o teto vale para a plataforma inteira, então é
		// informação de admin, não de produtor.
		"limite_avaliacao": map[string]any{
			"teto": subaccount.MaxAccounts, "alerta_em": subaccount.AlertAtAccounts,
			"contas_criadas": aprovadas + aguardando,
		},
	})
}

// adminSyncDocuments rebusca as pendências de documentação de um produtor. Serve para o
// caso em que um documento foi reprovado e o gateway gerou um link novo — sem isso, o
// produtor ficaria olhando um link morto.
func (s *Server) adminSyncDocuments(w http.ResponseWriter, r *http.Request, _ *auth.AdminClaims) {
	producerID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	acc, err := s.seams.Subaccounts.SyncDocuments(r.Context(), producerID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "onboarding_url": acc.OnboardingURL, "account_status": acc.Status,
	})
}

// adminSplits lista os repasses que precisam de gente: bloqueados por divergência,
// cancelados e recusados. O que liquidou sozinho não aparece — fila de trabalho mostra
// trabalho, não histórico.
func (s *Server) adminSplits(w http.ResponseWriter, r *http.Request, _ *auth.AdminClaims) {
	producers, err := store.ListProducers(r.Context(), s.pool, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type row struct {
		ProducerID   uuid.UUID `json:"producer_id"`
		ProducerName string    `json:"producer_name"`
		OrderID      uuid.UUID `json:"order_id"`
		EventTitle   string    `json:"event_title"`
		FaceCents    int64     `json:"face_cents"`
		Method       string    `json:"payment_method"`
		Installments int       `json:"installments"`
		Status       string    `json:"split_status"`
		Reason       string    `json:"refusal_reason,omitempty"`
		AsaasPayment string    `json:"asaas_payment_id,omitempty"`
		UpdatedAt    time.Time `json:"updated_at"`
	}
	list := []row{}
	for _, p := range producers {
		if err := s.withTenant(r.Context(), p.ID, func(tx pgx.Tx) error {
			rows, e := tx.Query(r.Context(), `
				SELECT st.order_id, COALESCE(e.title,''), st.face_cents, st.payment_method,
				       st.installments, st.split_status, COALESCE(st.refusal_reason,''),
				       COALESCE(st.asaas_payment_id,''), st.updated_at
				  FROM split_transfers st
				  LEFT JOIN events e ON e.id = st.event_id
				 WHERE st.split_status IN ($1,$2,$3)
				 ORDER BY st.updated_at DESC`,
				checkout.SplitBlocked, checkout.SplitCancelled, checkout.SplitRefused)
			if e != nil {
				return e
			}
			defer rows.Close()
			for rows.Next() {
				it := row{ProducerID: p.ID, ProducerName: p.Name}
				if e := rows.Scan(&it.OrderID, &it.EventTitle, &it.FaceCents, &it.Method,
					&it.Installments, &it.Status, &it.Reason, &it.AsaasPayment, &it.UpdatedAt); e != nil {
					return e
				}
				list = append(list, it)
			}
			return rows.Err()
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"splits": list})
}

// adminLineup mostra e edita o rateio informativo do line-up de um evento. Fica no admin
// porque hoje é daqui que a operação acompanha os eventos — e o rateio não move dinheiro,
// então editá-lo não é ato financeiro.
func (s *Server) adminLineup(w http.ResponseWriter, r *http.Request, _ *auth.AdminClaims) {
	producerID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "produtor inválido")
		return
	}
	eventID, err := uuid.Parse(r.PathValue("eventId"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "evento inválido")
		return
	}
	if r.Method == http.MethodGet {
		var shares []checkout.LineupShare
		if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
			var e error
			shares, e = checkout.LineupPreview(r.Context(), tx, eventID)
			return e
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if shares == nil {
			shares = []checkout.LineupShare{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"shares": shares})
		return
	}

	var body lineupReq
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "corpo inválido")
		return
	}
	var total float64
	for _, sh := range body.Shares {
		total += sh.SharePct
	}
	if total > 100 {
		writeErr(w, http.StatusBadRequest, "a soma do line-up passa de 100% do valor de face")
		return
	}
	if err := s.withTenant(r.Context(), producerID, func(tx pgx.Tx) error {
		if _, e := tx.Exec(r.Context(), `DELETE FROM lineup_shares WHERE event_id=$1`, eventID); e != nil {
			return e
		}
		for _, sh := range body.Shares {
			if _, e := tx.Exec(r.Context(), `
				INSERT INTO lineup_shares (event_id, artist_name, share_pct) VALUES ($1,$2,$3)`,
				eventID, sh.ArtistName, sh.SharePct); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
