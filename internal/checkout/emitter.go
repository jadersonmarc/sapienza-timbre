package checkout

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jadersonmarc/sapienza-timbre/internal/chain"
	"github.com/jadersonmarc/sapienza-timbre/internal/nft"
	"github.com/jadersonmarc/sapienza-timbre/internal/notify"
	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
)

// Emitter assina os ingressos emitidos (Ed25519), gera os metadados públicos ERC-1155,
// entrega o QR ao comprador e enfileira a emissão on-chain. É injetado no confirm/
// cortesia para não acoplar o checkout à chave privada nem à rede. ProducerID é usado
// nos metadados públicos (resolução por ticket_id).
type Emitter struct {
	Signer     *ticketing.Signer
	Notify     notify.Notifier
	Chain      chain.ChainDriver
	MintMode   chain.MintMode // on_demand (não enfileira) | eager (enfileira no pagamento)
	ProducerID uuid.UUID
}

// EmitTickets assina + entrega + enfileira o mint de um conjunto de ingressos. Exposto
// para outros fluxos de emissão (passe de temporada, cortesias) reusarem.
func (e Emitter) EmitTickets(ctx context.Context, tx pgx.Tx, ticketIDs []uuid.UUID, deliverTo string) error {
	return e.emit(ctx, tx, ticketIDs, deliverTo)
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
		// Metadados públicos ERC-1155 (sem dado pessoal).
		if err := nft.GenerateMetadata(ctx, tx, e.ProducerID, tid); err != nil {
			return err
		}
		// Emissão on-chain: só no modo eager (no on_demand, a materialização é sob demanda).
		if e.MintMode == chain.MintModeEager && e.Chain != nil && e.Chain.Enabled() {
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
