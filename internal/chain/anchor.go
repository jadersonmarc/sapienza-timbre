package chain

import (
	"context"
	"encoding/hex"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AnchorMode controla a âncora do atestado. Não existe cliente HTTP de Relayer ainda:
// off (default) não faz nada; log registra a intenção sem marcar 'anchored'.
type AnchorMode string

const (
	// AnchorModeOff: nenhuma âncora (default).
	AnchorModeOff AnchorMode = "off"
	// AnchorModeLog: registra a intenção de ancorar; o atestado permanece anchor_status 'none'.
	AnchorModeLog AnchorMode = "log"
)

// ValidAnchorMode diz se m é um modo conhecido.
func ValidAnchorMode(m string) bool {
	return m == string(AnchorModeOff) || m == string(AnchorModeLog)
}

// ReasonAttestation é a razão do chain_job de âncora.
const ReasonAttestation = "attestation"

// Anchorer envia o resumo do atestado para a cadeia. A âncora NUNCA bloqueia o fechamento.
// A implementação real (transação com o resumo no campo de dados) chega junto com o modo
// real; hoje só existem Noop (off) e Log (log) — nenhuma marca 'anchored' sem transação.
type Anchorer interface {
	Enabled() bool
	SendAnchor(ctx context.Context, digest []byte) (txHash string, err error)
}

// NoopAnchorer é o default (modo off).
type NoopAnchorer struct{}

func (NoopAnchorer) Enabled() bool { return false }
func (NoopAnchorer) SendAnchor(context.Context, []byte) (string, error) {
	return "", nil
}

// LogAnchorer marca o modo 'log': registra a intenção. Enabled é false — o worker nunca
// processa, então nada vira 'anchored' sem transação real.
type LogAnchorer struct{}

func (LogAnchorer) Enabled() bool { return false }
func (LogAnchorer) SendAnchor(ctx context.Context, digest []byte) (string, error) {
	slog.Info("anchor intent", "digest", hex.EncodeToString(digest))
	return "", nil
}

// EnqueueAnchor enfileira a reancoragem manual de um atestado (zera tentativas de um job
// anterior) e marca anchor_status 'pending'. Roda na MESMA transação do chamador.
func EnqueueAnchor(ctx context.Context, tx pgx.Tx, attestationID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `
		UPDATE chain_jobs SET status='pending', attempts=0, last_error=NULL, next_attempt_at=now(), updated_at=now()
		 WHERE attestation_id=$1 AND kind='anchor'`, attestationID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO chain_jobs (attestation_id, kind, reason)
			VALUES ($1,'anchor','attestation')`, attestationID); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE event_attestations SET anchor_status='pending' WHERE id=$1`, attestationID)
	return err
}
