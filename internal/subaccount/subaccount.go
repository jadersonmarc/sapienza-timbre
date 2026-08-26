// Package subaccount cuida da conta de recebimento do produtor no gateway (subconta
// padrão — não é BaaS, não é conta escrow). É ela que recebe o split de cada venda.
//
// A conta pertence ao PRODUTOR e é reusada em todos os eventos dele. O ciclo de vida é
// independente do evento:
//
//	sem_conta → criada_aguardando_docs → em_analise → aprovada | reprovada
//
// Evento pode ser criado, editado e configurado em qualquer estado. Só ABRIR VENDA exige
// aprovada — o produtor monta o evento enquanto a análise corre.
package subaccount

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
)

// Estados da conta.
const (
	StatusNone      = "sem_conta"
	StatusAwaitDocs = "criada_aguardando_docs"
	StatusAnalysis  = "em_analise"
	StatusApproved  = "aprovada"
	StatusRejected  = "reprovada"
)

// Limites do período de avaliação regulatória. Enquanto ele corre, a plataforma pode criar
// no máximo MaxAccounts subcontas de titulares distintos, em ReviewWindow a partir da
// primeira. Estourar bloqueia criação e emissão — por isso o alerta antecipado.
const (
	MaxAccounts     = 10
	AlertAtAccounts = 7
	ReviewWindow    = 60 * 24 * time.Hour
)

var (
	// ErrLimitReached: teto do período de avaliação regulatória atingido.
	ErrLimitReached = errors.New("subaccount: teto de subcontas do período de avaliação atingido")
	// ErrDocumentTaken: o documento já tem subconta em outro produtor.
	ErrDocumentTaken = errors.New("subaccount: documento já vinculado a outra conta")
	// ErrNotFound: produtor sem conta.
	ErrNotFound = errors.New("subaccount: produtor sem conta de recebimento")
)

// Account é a conta de recebimento como o Timbre a conhece.
type Account struct {
	ProducerID              uuid.UUID  `json:"producer_id"`
	TaxID                   string     `json:"cpf_cnpj"`
	PersonType              string     `json:"person_type"`
	WalletID                string     `json:"wallet_id"`
	Status                  string     `json:"account_status"`
	StatusReason            string     `json:"status_reason,omitempty"`
	CommercialInfoExpiresAt *time.Time `json:"commercial_info_expires_at,omitempty"`
	OnboardingURL           string     `json:"onboarding_url,omitempty"`
}

// CanSell diz se a conta permite abrir venda.
func (a Account) CanSell() bool { return a.Status == StatusApproved && a.WalletID != "" }

// Service cria e acompanha as subcontas.
type Service struct {
	pool *pgxpool.Pool
	gw   payment.PaymentGateway
	// DocumentsDelay é a espera entre criar a conta e consultar os documentos: a validação
	// do documento com a Receita ainda não terminou, e consultar antes devolve pendências
	// erradas. Campo para o teste não esperar de verdade.
	DocumentsDelay time.Duration
}

// New constrói o serviço.
func New(pool *pgxpool.Pool, gw payment.PaymentGateway) *Service {
	return &Service{pool: pool, gw: gw, DocumentsDelay: 15 * time.Second}
}

// Get devolve a conta do produtor. ErrNotFound = estado sem_conta.
func (s *Service) Get(ctx context.Context, producerID uuid.UUID) (Account, error) {
	return get(ctx, s.pool, producerID)
}

func get(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, producerID uuid.UUID) (Account, error) {
	var a Account
	err := q.QueryRow(ctx, `
		SELECT producer_id, cpf_cnpj, person_type, wallet_id, account_status,
		       COALESCE(status_reason,''), commercial_info_expires_at, COALESCE(onboarding_url,'')
		  FROM producer_asaas_accounts WHERE producer_id=$1`, producerID).
		Scan(&a.ProducerID, &a.TaxID, &a.PersonType, &a.WalletID, &a.Status,
			&a.StatusReason, &a.CommercialInfoExpiresAt, &a.OnboardingURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{Status: StatusNone}, ErrNotFound
	}
	return a, err
}

// Create abre a subconta do produtor no gateway e registra o walletId.
//
// Concorrência: dois eventos publicados quase ao mesmo tempo (ou um duplo clique) disparam
// duas criações. O unique constraint sozinho não basta, porque a chamada externa acontece
// ANTES do insert — duas contas nasceriam no gateway e só uma seria gravada. Por isso o
// advisory lock por produtor em volta do bloco inteiro.
func (s *Service) Create(ctx context.Context, producerID uuid.UUID, in payment.AccountInput) (Account, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return Account{}, err
	}
	defer conn.Release()

	// Lock na mesma conexão que faz a verificação e o insert.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey(producerID)); err != nil {
		return Account{}, err
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, lockKey(producerID))
	}()

	// Já criada enquanto esperávamos o lock: o segundo clique não cria nada.
	if existing, err := get(ctx, conn, producerID); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Account{}, err
	}

	// Mesmo documento em outro produtor (sócios da mesma produtora): compartilham a conta
	// em vez de tentar criar a segunda, que o gateway recusaria.
	var otherProducer uuid.UUID
	var otherWallet, otherStatus, otherType string
	err = conn.QueryRow(ctx, `
		SELECT producer_id, wallet_id, account_status, person_type
		  FROM producer_asaas_accounts WHERE cpf_cnpj=$1`, in.TaxID).
		Scan(&otherProducer, &otherWallet, &otherStatus, &otherType)
	if err == nil {
		slog.Info("subaccount: documento já tem conta — produtores compartilham a subconta",
			"producer_id", producerID, "conta_de", otherProducer)
		return s.link(ctx, conn, producerID, in.TaxID, otherType, otherWallet, otherStatus)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Account{}, err
	}

	if err := s.checkReviewLimit(ctx, conn); err != nil {
		return Account{}, err
	}

	created, err := s.gw.CreateAccount(ctx, in)
	if err != nil {
		return Account{}, err
	}
	personType := "PJ"
	if in.CompanyType == "" {
		personType = "PF"
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO producer_asaas_accounts
		    (producer_id, cpf_cnpj, person_type, wallet_id, account_status, commercial_info_expires_at)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		producerID, in.TaxID, personType, created.WalletID, StatusAwaitDocs, created.CommercialInfoExpiresAt); err != nil {
		return Account{}, fmt.Errorf("gravar conta de recebimento: %w", err)
	}
	// Espelha no produtor: é de lá que o checkout lê o destinatário do split.
	if _, err := conn.Exec(ctx, `UPDATE producers SET asaas_wallet_id=$2, updated_at=now() WHERE id=$1`,
		producerID, created.WalletID); err != nil {
		return Account{}, err
	}
	return get(ctx, conn, producerID)
}

// link vincula o produtor a uma subconta que já existe para o mesmo documento.
func (s *Service) link(ctx context.Context, conn *pgxpool.Conn, producerID uuid.UUID, taxID, personType, walletID, status string) (Account, error) {
	if _, err := conn.Exec(ctx, `
		INSERT INTO producer_asaas_accounts (producer_id, cpf_cnpj, person_type, wallet_id, account_status)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (producer_id) DO UPDATE SET wallet_id=EXCLUDED.wallet_id, account_status=EXCLUDED.account_status`,
		producerID, taxID+":"+producerID.String(), personType, walletID, status); err != nil {
		return Account{}, err
	}
	if _, err := conn.Exec(ctx, `UPDATE producers SET asaas_wallet_id=$2, updated_at=now() WHERE id=$1`, producerID, walletID); err != nil {
		return Account{}, err
	}
	return get(ctx, conn, producerID)
}

// SyncDocuments consulta as pendências de documentação e guarda o link de onboarding. Deve
// rodar DEPOIS da espera: consultar antes de a validação do documento terminar devolve
// pendências incorretas.
func (s *Service) SyncDocuments(ctx context.Context, producerID uuid.UUID) (Account, error) {
	acc, err := s.Get(ctx, producerID)
	if err != nil {
		return Account{}, err
	}
	docs, err := s.gw.AccountDocuments(ctx, acc.WalletID)
	if err != nil {
		return acc, err
	}
	first := ""
	for _, d := range docs.Items {
		if first == "" && d.OnboardingURL != "" {
			first = d.OnboardingURL
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO producer_asaas_documents (producer_id, document_id, document_type, status, onboarding_url)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (producer_id, document_id)
			DO UPDATE SET status=EXCLUDED.status, onboarding_url=EXCLUDED.onboarding_url, updated_at=now()`,
			producerID, d.ID, d.Type, d.Status, d.OnboardingURL); err != nil {
			return acc, err
		}
	}
	if first != "" {
		if _, err := s.pool.Exec(ctx, `
			UPDATE producer_asaas_accounts SET onboarding_url=$2, updated_at=now() WHERE producer_id=$1`,
			producerID, first); err != nil {
			return acc, err
		}
		acc.OnboardingURL = first
	}
	return acc, nil
}

// SetStatus move a máquina de estados (webhook de situação da conta).
func (s *Service) SetStatus(ctx context.Context, walletID, status, reason string) error {
	if !validStatus(status) {
		return fmt.Errorf("status de conta desconhecido: %q", status)
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE producer_asaas_accounts
		   SET account_status=$2, status_reason=NULLIF($3,''), updated_at=now()
		 WHERE wallet_id=$1`, walletID, status, reason)
	return err
}

// SetCommercialInfoExpiration guarda a data da próxima confirmação anual (vem do gateway,
// por webhook ou na criação — nunca por polling: o campo só muda uma vez por ano).
func (s *Service) SetCommercialInfoExpiration(ctx context.Context, walletID string, at *time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE producer_asaas_accounts SET commercial_info_expires_at=$2, updated_at=now()
		 WHERE wallet_id=$1`, walletID, at)
	return err
}

// ConfirmCommercialInfo submete a confirmação anual de dados comerciais.
func (s *Service) ConfirmCommercialInfo(ctx context.Context, producerID uuid.UUID, in payment.AccountInput) error {
	acc, err := s.Get(ctx, producerID)
	if err != nil {
		return err
	}
	confirmer, ok := s.gw.(interface {
		ConfirmCommercialInfo(context.Context, string, payment.AccountInput) error
	})
	if !ok {
		return fmt.Errorf("gateway atual não confirma dados comerciais")
	}
	return confirmer.ConfirmCommercialInfo(ctx, acc.WalletID, in)
}

// checkReviewLimit barra a criação quando o teto do período de avaliação regulatória foi
// atingido, e avisa antes de chegar lá.
func (s *Service) checkReviewLimit(ctx context.Context, conn interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) error {
	var total int
	var first *time.Time
	if err := conn.QueryRow(ctx, `
		SELECT count(*), min(created_at) FROM producer_asaas_accounts
		 WHERE cpf_cnpj NOT LIKE 'legado:%'`).Scan(&total, &first); err != nil {
		return err
	}
	// A janela conta a partir da primeira subconta criada via API.
	if first != nil && time.Since(*first) > ReviewWindow {
		slog.Error("subaccount: janela de avaliação regulatória expirada — criação e emissão bloqueadas",
			"primeira_conta", first.Format(time.RFC3339), "janela_dias", int(ReviewWindow.Hours()/24))
		return ErrLimitReached
	}
	if total >= MaxAccounts {
		slog.Error("subaccount: teto de subcontas do período de avaliação atingido", "total", total, "teto", MaxAccounts)
		return ErrLimitReached
	}
	if total+1 >= AlertAtAccounts {
		slog.Warn("subaccount: aproximando do teto do período de avaliação regulatória",
			"total_apos_esta", total+1, "teto", MaxAccounts)
	}
	return nil
}

func validStatus(s string) bool {
	switch s {
	case StatusAwaitDocs, StatusAnalysis, StatusApproved, StatusRejected:
		return true
	}
	return false
}

// lockKey deriva a chave do advisory lock do id do produtor.
func lockKey(id uuid.UUID) int64 {
	h := fnv.New64a()
	_, _ = h.Write(id[:])
	return int64(h.Sum64() >> 1) // positivo, para não colidir com convenções de chave negativa
}
