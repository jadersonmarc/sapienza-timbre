package checkout

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/chain"
	"github.com/jadersonmarc/sapienza-timbre/internal/notify"
	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
)

// Emitter assina os ingressos emitidos (Ed25519), entrega o QR ao comprador e enfileira
// a emissão on-chain. É injetado no confirm/cortesia para não acoplar o checkout à
// chave privada nem à rede.
type Emitter struct {
	Signer *ticketing.Signer
	Notify notify.Notifier
	Chain  chain.ChainDriver
}

// emit assina cada ingresso e, havendo destinatário, entrega o token do QR. A entrega
// é só para o comprador (o QR é a credencial de entrada — nunca exposto por id público).
func (e Emitter) emit(ctx context.Context, tx pgx.Tx, ticketIDs []uuid.UUID, deliverTo string) error {
	if e.Signer == nil {
		return nil // sem chave configurada: ingresso fica sem assinatura (dev)
	}
	for _, tid := range ticketIDs {
		if err := ticketing.SignTicket(ctx, tx, e.Signer, tid); err != nil {
			return err
		}
		// Enfileira o mint on-chain (em segundo plano) só se há rede de verdade.
		if e.Chain != nil && e.Chain.Enabled() {
			if err := chain.EnqueueMint(ctx, tx, tid); err != nil {
				return err
			}
		}
		if e.Notify == nil || deliverTo == "" {
			continue
		}
		token, err := ticketing.TicketToken(ctx, tx, tid)
		if err != nil {
			return err
		}
		_ = e.Notify.Send(ctx, notify.Message{
			Channel: "email", To: deliverTo, Subject: "Seu ingresso Timbre", Body: token,
		})
	}
	return nil
}
