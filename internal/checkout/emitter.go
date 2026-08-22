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
		// Metadados públicos (sem dado pessoal). A emissão on-chain (mint) está DESATIVADA:
		// o eixo on-chain agora é prova por âncora, não posse por token.
		if err := nft.GenerateMetadata(ctx, tx, e.ProducerID, tid); err != nil {
			return err
		}
		if e.Notify == nil || deliverTo == "" {
			continue
		}
		token, err := ticketing.TicketToken(ctx, tx, tid)
		if err != nil {
			return err
		}
		info, err := loadTicketEmailInfo(ctx, tx, tid)
		if err != nil {
			return err
		}
		_ = e.Notify.Send(ctx, notify.Message{
			Kind: notify.KindTicket, Channel: "email", To: deliverTo,
			ProducerID: &e.ProducerID, EventID: &info.eventID, TicketID: &tid,
			EventName: info.title, EventStarts: info.starts,
			VenueCity: info.city, Address: info.address,
			SectorName: info.sector, SeatLabel: info.seat,
			QRContent: token,
		})
	}
	return nil
}

// ticketEmailInfo carrega os dados do evento/assento para a mensagem de ingresso (uma
// mensagem por ingresso — quem compra quatro repassa três).
type ticketEmailInfo struct {
	eventID uuid.UUID
	title   string
	starts  string
	city    string
	address string
	sector  string
	seat    string
}

func loadTicketEmailInfo(ctx context.Context, tx pgx.Tx, ticketID uuid.UUID) (ticketEmailInfo, error) {
	var i ticketEmailInfo
	err := tx.QueryRow(ctx, `
		SELECT t.event_id, e.title, to_char(e.starts_at, 'DD/MM/YYYY HH24:MI'),
		       COALESCE(e.city,''), COALESCE(e.address,''),
		       COALESCE(se.name,''), COALESCE(s.row_label,'') || COALESCE(s.number,'')
		  FROM tickets t
		  JOIN events e ON e.id = t.event_id
		  LEFT JOIN seats s ON s.id = t.seat_id
		  LEFT JOIN sectors se ON se.id = s.sector_id
		 WHERE t.id = $1`, ticketID).
		Scan(&i.eventID, &i.title, &i.starts, &i.city, &i.address, &i.sector, &i.seat)
	return i, err
}
