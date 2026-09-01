package api

import (
	"encoding/csv"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/attest"
	"github.com/jadersonmarc/sapienza-timbre/internal/audit"
	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/dash"
	"github.com/jadersonmarc/sapienza-timbre/internal/notify"
	"github.com/jadersonmarc/sapienza-timbre/internal/payout"
	"github.com/jadersonmarc/sapienza-timbre/internal/store"
)

// dashOverview devolve, em uma chamada, o que o painel do produtor mostra em tempo
// real: curva por lote, ocupação, andamento do check-in e financeiro do evento.
func (s *Server) dashOverview(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var (
		sales  []dash.LotSales
		occ    dash.Occupancy
		chk    dash.Checkin
		fin    dash.Finance
		funnel dash.SessionFunnel
		half   attest.HalfPriceAllowance
		pay    payout.Payout
	)
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		if sales, e = dash.SalesByLot(r.Context(), tx, eventID); e != nil {
			return e
		}
		if occ, e = dash.EventOccupancy(r.Context(), tx, eventID); e != nil {
			return e
		}
		if chk, e = dash.CheckinProgress(r.Context(), tx, eventID); e != nil {
			return e
		}
		if fin, e = dash.EventFinance(r.Context(), tx, eventID); e != nil {
			return e
		}
		if funnel, e = dash.EventSessionFunnel(r.Context(), tx, eventID); e != nil {
			return e
		}
		// O repasse entra aqui porque é a pergunta que o produtor faz junto com "quanto
		// vendi": com a bilheteria retendo até depois do evento, não mostrar quanto ele tem
		// a receber e quando faz o modelo ser indistinguível de "a plataforma está com o meu
		// dinheiro e não me explica nada". Recalculado na leitura — o extrato é em tempo real.
		if pay, e = payout.Recompute(r.Context(), tx, eventID); e != nil {
			return e
		}
		// A cota de meia entra no painel porque ela BARRA venda desde que passou a valer: o
		// produtor precisa ver quanto resta no mesmo lugar em que acompanha as vendas.
		half, e = attest.HalfPrice(r.Context(), tx, eventID)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sales": sales, "occupancy": occ, "checkin": chk, "finance": fin,
		"session_funnel": funnel, "half_price": half, "payout": pay,
	})
}

// dashSummary é o resumo do produtor (todos os eventos).
func (s *Server) dashSummary(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	var sum dash.ProducerSummary
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		var e error
		sum, e = dash.Summary(r.Context(), tx)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// dashPayouts é o extrato de repasses do produtor: cada evento, quanto ele tem a receber,
// quando, e em que pé está.
//
// Sem esta tela o modelo de retenção é indistinguível de "a plataforma está com o meu
// dinheiro e não me explica nada" — que é exatamente a suspeita que uma bilheteria precisa
// não merecer.
func (s *Server) dashPayouts(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	var rows []payout.Payout
	var credits []recoverableCredit
	if err := s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		// Recalcula antes de listar: o produtor abre esta tela justamente depois de uma
		// venda ou de um estorno, e um número velho aqui é pior que número nenhum.
		ids, e := payout.EventIDs(r.Context(), tx)
		if e != nil {
			return e
		}
		for _, id := range ids {
			if _, e := payout.Recompute(r.Context(), tx, id); e != nil {
				return e
			}
		}
		if rows, e = payout.List(r.Context(), tx); e != nil {
			return e
		}
		credits, e = openCredits(r.Context(), tx)
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var pending, upcoming, paid int64
	for _, p := range rows {
		switch p.Status {
		case payout.StatusPending:
			pending += p.NetDueCents
		case payout.StatusAccruing:
			// Ainda não venceu porque o evento não aconteceu. Somar junto com o vencido
			// diria ao produtor que ele tem a receber hoje o que só recebe depois da festa.
			upcoming += p.NetDueCents
		case payout.StatusPaid:
			paid += p.NetDueCents
		}
	}
	acc, err := store.GetPayoutAccount(r.Context(), s.pool, claims.ProducerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"payouts":             rows,
		"pending_cents":       pending,
		"upcoming_cents":      upcoming,
		"paid_cents":          paid,
		"destination_missing": acc.PixKey == "",
		// Créditos a recuperar: estorno que chegou depois de o repasse ter sido pago. O
		// produtor vê porque a cobrança vai chegar nele — e chegar de surpresa é pior.
		"recoverable_credits": credits,
	})
}

// dashNotifications mostra os envios de um evento (ingressos) — quantos foram, quantos
// falharam e a lista para reenviar. É a primeira coisa que o produtor procura quando
// alguém diz que não recebeu o ingresso.
func (s *Server) dashNotifications(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT id, kind, to_email, status, created_at,
		       COALESCE(provider_message_id,''), COALESCE(last_error,'')
		  FROM public.notifications
		 WHERE event_id=$1 AND kind IN ('ticket_issued','order_refunded')
		 ORDER BY created_at DESC LIMIT 200`, eventID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	type n struct {
		ID        uuid.UUID `json:"id"`
		Kind      string    `json:"kind"`
		ToEmail   string    `json:"to_email"`
		Status    string    `json:"status"`
		CreatedAt time.Time `json:"created_at"`
		// Delivered separa "saiu de verdade" de "só foi registrado": no modo log o id do
		// provedor é a string "log" e o status também é 'sent'.
		Delivered bool   `json:"delivered"`
		LastError string `json:"last_error,omitempty"`
	}
	var list []n
	sent, failed, logged := 0, 0, 0
	for rows.Next() {
		var it n
		var providerID string
		if err := rows.Scan(&it.ID, &it.Kind, &it.ToEmail, &it.Status, &it.CreatedAt, &providerID, &it.LastError); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		it.Delivered = it.Status == "sent" && providerID != "" && providerID != "log"
		switch {
		case it.Delivered:
			sent++
		case it.Status == "sent":
			logged++
		case it.Status == "failed":
			failed++
		}
		list = append(list, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sent": sent, "failed": failed, "logged_only": logged, "notifications": list})
}

// resendNotification reenfileira um envio que falhou (painel). Não duplica ingresso — só a
// mensagem. Verifica que a notificação pertence ao produtor.
func (s *Server) resendNotification(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var producerID uuid.UUID
	if err := s.pool.QueryRow(r.Context(), `SELECT producer_id FROM public.notifications WHERE id=$1`, id).Scan(&producerID); err != nil {
		writeErr(w, http.StatusNotFound, "notificação não encontrada")
		return
	}
	if producerID != claims.ProducerID {
		writeErr(w, http.StatusNotFound, "notificação não encontrada")
		return
	}
	newID, err := notify.ResendNotification(r.Context(), s.pool, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "notification_id": newID})
}

// dashExportCSV exporta os ingressos do evento em CSV, uma linha por ingresso.
//
// A resposta é STREAMING: a query percorre o cursor e cada linha vai para a rede na hora.
// Não existe fila nem arquivo intermediário porque não há o que esperar — o custo não cresce
// com o tamanho do evento, e um link para um arquivo que precisaria ser guardado em algum
// lugar seria um mecanismo novo (armazenamento, expiração, permissão de leitura) para
// resolver um problema que o streaming já não tem.
//
// Por consequência, o erro no meio da varredura chega DEPOIS do cabeçalho 200. Fecha-se a
// linha com um marcador de erro em vez de fingir que o arquivo terminou: planilha truncada
// em silêncio é pior que planilha que se acusa incompleta.
//
// A exportação leva dado de comprador para fora do sistema, então ela é registrada na
// trilha — quem exportou, quando, de qual evento e com qual recorte.
func (s *Server) dashExportCSV(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	eventID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	q := r.URL.Query()
	var f dash.ExportFilter
	f.Status = q.Get("status")
	if v := q.Get("from"); v != "" {
		if t, e := time.Parse("2006-01-02", v); e == nil {
			f.From = t
		}
	}
	if v := q.Get("to"); v != "" {
		if t, e := time.Parse("2006-01-02", v); e == nil {
			// Data final é INCLUSIVA: quem digita 31/08 quer o dia 31 inteiro.
			f.To = t.Add(24*time.Hour - time.Second)
		}
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="ingressos.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write(dash.ExportHeader())
	flusher, _ := w.(http.Flusher)

	n := 0
	err = s.withTenant(r.Context(), claims.ProducerID, func(tx pgx.Tx) error {
		if e := dash.StreamTicketsForExport(r.Context(), tx, eventID, f, func(row dash.TicketRow) error {
			if e := cw.Write(row.Fields()); e != nil {
				return e
			}
			n++
			if n%500 == 0 {
				cw.Flush()
				if flusher != nil {
					flusher.Flush()
				}
			}
			return nil
		}); e != nil {
			return e
		}
		// A trilha entra na MESMA transação da leitura: exportação que falhou no meio não
		// deixa registro de uma entrega que não aconteceu inteira.
		return audit.Append(r.Context(), tx, audit.Event{
			Entity: audit.EntityExport, ActorKind: audit.ActorProducer, Actor: claims.Subject,
			ToStatus: "exported",
			Details: map[string]any{
				"event_id": eventID.String(), "rows": n,
				"from": q.Get("from"), "to": q.Get("to"), "status": f.Status,
			},
		})
	})
	if err != nil {
		// Cabeçalho já foi. Marca a planilha como incompleta em vez de terminar calado.
		_ = cw.Write([]string{"ERRO", "exportação interrompida — refaça", err.Error()})
	}
	cw.Flush()
}
