// Command server é o binário do Timbre: sobe config, conecta ao Postgres (banco
// próprio), aplica a camada compartilhada `public`, faz catch-up das migrations de
// tenant, e serve /health + /api/v1. Data plane no espírito Control/Data da Margot,
// mas com tenancy de produtor e auth nativas.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-timbre/db/migrations"
	"github.com/jadersonmarc/sapienza-timbre/internal/api"
	"github.com/jadersonmarc/sapienza-timbre/internal/attest"
	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/chain"
	"github.com/jadersonmarc/sapienza-timbre/internal/checkout"
	"github.com/jadersonmarc/sapienza-timbre/internal/config"
	"github.com/jadersonmarc/sapienza-timbre/internal/db"
	"github.com/jadersonmarc/sapienza-timbre/internal/gateweb"
	"github.com/jadersonmarc/sapienza-timbre/internal/inventory"
	"github.com/jadersonmarc/sapienza-timbre/internal/ledger"
	"github.com/jadersonmarc/sapienza-timbre/internal/notify"
	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
	"github.com/jadersonmarc/sapienza-timbre/internal/producer"
	"github.com/jadersonmarc/sapienza-timbre/internal/store"
	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
)

func main() {
	ctx := context.Background()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel(cfg.LogLevel)})))

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer pool.Close()

	// Camada compartilhada do Timbre (control plane + identidade/audiência) em `public`.
	if err := db.MigratePublic(ctx, pool, migrations.Public); err != nil {
		log.Fatalf("migrate public: %v", err)
	}

	// Catch-up idempotente das migrations de tenant nos produtores já existentes.
	runner := tenancy.NewMigrationRunner(pool, migrations.Tenant)
	if err := runner.ApplyToAllTenants(ctx); err != nil {
		log.Fatalf("migrate tenants (catch-up): %v", err)
	}

	// Seed do primeiro super_admin do painel /admin (idempotente). Sem ele, nenhum
	// operador consegue logar. O X-Admin-Token continua só para o bootstrap de produtor.
	if email := os.Getenv("TIMBRE_ADMIN_EMAIL"); email != "" {
		if pass := os.Getenv("TIMBRE_ADMIN_PASSWORD"); pass == "" {
			log.Fatalf("TIMBRE_ADMIN_EMAIL definido sem TIMBRE_ADMIN_PASSWORD")
		}
		if err := seedSuperAdmin(ctx, pool, email, os.Getenv("TIMBRE_ADMIN_PASSWORD")); err != nil {
			log.Fatalf("seed super admin: %v", err)
		}
	}

	// Gateway de pagamento: Asaas real se houver credencial, senão o Fake (default).
	var pay payment.PaymentGateway = payment.NewFakeGateway()
	payKind := "fake"
	if key := os.Getenv("ASAAS_API_KEY"); key != "" {
		asaas := payment.NewAsaas(key, os.Getenv("ASAAS_BASE_URL"))
		pay = asaas
		payKind = "asaas"
		// A base efetiva no log: uma base errada autentica e devolve 404 na cobrança, e sem
		// esta linha não há como ver para onde as chamadas foram.
		slog.Info("payment: asaas", "base_url", asaas.BaseURL())
	}
	// Rede: Base se configurada (RPC + contrato), senão Noop (chain desligada — a venda
	// nunca depende de rede). A emissão on-chain roda numa fila em segundo plano.
	var chainDriver chain.ChainDriver = chain.NoopChainDriver{}
	chainKind := "noop"
	if rpc := os.Getenv("CHAIN_RPC_URL"); rpc != "" {
		chainDriver = chain.NewBase(rpc, os.Getenv("CHAIN_CONTRACT"))
		chainKind = "base"
	}
	// Notificação: envio ASSÍNCRONO. O Service enfileira em public.notifications; o worker
	// drena a fila e envia pelo provedor (Resend ou log). O caminho de venda/código nunca
	// espera o envio. A chave de API nunca aparece em log.
	notifier := notify.NewService(pool, cfg.PublicBaseURL) // implementa notify.Notifier (enfileira)
	var provider notify.Provider = notify.LogProvider{}
	notifyKind := "log"
	if cfg.Notifier == "resend" {
		provider = notify.NewResendProvider(cfg.ResendAPIKey, cfg.MailFrom, cfg.MailReplyTo)
		notifyKind = "resend"
		slog.Info("notify: envio real ligado", "provider", "resend", "from", cfg.MailFrom)
		// Remetente sem endereço é recusado pelo provedor e TODA mensagem falha. O caso
		// real: MAIL_FROM="Nome <caixa@dominio>" sem aspas — o shell corta no '<' e sobra
		// só o nome. O aviso nomeia o defeito; o serviço segue de pé (venda não depende
		// de e-mail).
		if !strings.Contains(cfg.MailFrom, "@") {
			slog.Error("notify: remetente inválido — nenhum e-mail será aceito pelo provedor; use MAIL_FROM=caixa@dominio ou MAIL_FROM=\"Nome <caixa@dominio>\" (com aspas)",
				"from", cfg.MailFrom)
		}
	} else {
		// Modo log entrega NADA. É um default legítimo em dev e um defeito em produção —
		// o aviso diz o que falta, porque a fila fica igual (status 'sent') nos dois casos.
		slog.Warn("notify: modo log — nenhum e-mail sai; defina RESEND_API_KEY e MAIL_FROM para enviar de verdade",
			"resend_api_key_set", cfg.ResendAPIKey != "", "mail_from_set", cfg.MailFrom != "")
	}
	go notify.NewWorker(pool, provider).Run(ctx)

	seams := api.Seams{
		Chain:   chainDriver,
		Payment: pay,
		Notify:  notifier,
	}
	slog.Info("seams", "chain", chainKind, "chain_enabled", chainDriver.Enabled(), "payment", payKind, "notify", notifyKind)

	// Varredura de expiração de holds (motor de reserva) por produtor, em segundo plano.
	go inventory.NewSweeper(pool).Run(ctx)
	// Fechamento de repasses (payouts) por produtor, em segundo plano.
	go ledger.NewSettler(pool).Run(ctx)

	// Chave de assinatura dos ingressos (Ed25519). Persistente em produção (a portaria
	// embarca a pública); efêmera em dev — o QR muda a cada restart.
	var signer *ticketing.Signer
	if cfg.TicketSigningKey != "" {
		if signer, err = ticketing.NewSigner(cfg.TicketSigningKey); err != nil {
			log.Fatalf("ticket signing key: %v", err)
		}
	} else {
		signer = ticketing.GenerateSigner()
		slog.Warn("TIMBRE_TICKET_SIGNING_KEY ausente — chave efêmera (QR muda a cada restart)")
	}
	slog.Info("ticketing", "public_key", signer.PublicKeyB64())

	// Chave de ATESTAÇÃO (Ed25519, propósito próprio, distinta da chave do QR). Assina o
	// resumo do registro canônico no fechamento do evento.
	var attestSigner *ticketing.Signer
	if cfg.AttestationKey != "" {
		if attestSigner, err = ticketing.NewSigner(cfg.AttestationKey); err != nil {
			log.Fatalf("attestation key: %v", err)
		}
	} else {
		attestSigner = ticketing.GenerateSigner()
		slog.Warn("TIMBRE_ATTESTATION_KEY ausente — chave de atestação efêmera (dev)")
	}
	slog.Info("attestation", "public_key", attestSigner.PublicKeyB64())

	// Identificador da chave de atestação: obrigatório quando a chave é definida (senão a
	// rotação invalidaria atestados antigos — a verificação resolve pelo key_id, nunca pela
	// chave corrente).
	attestKeyID := strings.TrimSpace(cfg.AttestationKeyID)
	if cfg.AttestationKey != "" && attestKeyID == "" {
		log.Fatalf("TIMBRE_ATTESTATION_KEY definida sem TIMBRE_ATTESTATION_KEY_ID")
	}
	if attestKeyID == "" {
		attestKeyID = "dev-" + base64.RawURLEncoding.EncodeToString(attestSigner.PublicKeyBytes())[:8]
		slog.Warn("TIMBRE_ATTESTATION_KEY_ID ausente — key_id efêmero (dev): " + attestKeyID)
	}
	// Registro idempotente da chave corrente em attestation_keys.
	if _, err := pool.Exec(ctx, `
		INSERT INTO public.attestation_keys (key_id, public_key, algorithm)
		VALUES ($1,$2,'ed25519') ON CONFLICT (key_id) DO NOTHING`,
		attestKeyID, attestSigner.PublicKeyB64()); err != nil {
		log.Fatalf("registrar chave de atestação: %v", err)
	}

	// Âncora do atestado: off (default) ou log. Modo desconhecido falha na inicialização.
	if !chain.ValidAnchorMode(cfg.AnchorMode) {
		log.Fatalf("TIMBRE_ANCHOR_MODE inválido: %q (use off ou log)", cfg.AnchorMode)
	}
	var anchorer chain.Anchorer = chain.NoopAnchorer{}
	anchorKind := "off"
	if cfg.AnchorMode == "log" {
		anchorer = chain.LogAnchorer{} // registra a intenção; nada vira 'anchored'
		anchorKind = "log"
	}
	slog.Info("anchor", "mode", cfg.AnchorMode, "kind", anchorKind)

	// Fechamento automático: N horas após ends_at (configurável).
	closeAfter := attest.DefaultCloseAfter
	if v := os.Getenv("EVENT_CLOSE_AFTER_HOURS"); v != "" {
		if h, err := strconv.Atoi(v); err == nil && h > 0 {
			closeAfter = time.Duration(h) * time.Hour
		}
	}

	// Fechamento automático de eventos (N horas após ends_at) e worker de âncora, em
	// segundo plano.
	go attest.NewCloser(pool, attestSigner, anchorer, chain.AnchorMode(cfg.AnchorMode), attestKeyID, closeAfter).Run(ctx)
	go attest.NewAnchorWorker(pool, anchorer).Run(ctx)
	// Limites da sessão de checkout (configuráveis por env, default calibrado para CGNAT).
	checkoutLimits := checkout.LimitsFromEnv()

	// Varredura de sessões de checkout vencidas (libera a reserva e purga client_ip), em
	// segundo plano.
	go checkout.NewSessionSweeper(pool, checkoutLimits).Run(ctx)

	authz := auth.New(cfg.JWTSecret)
	prov := producer.New(pool, runner)
	srv := api.NewServer(pool, authz, prov, signer, attestSigner, attestKeyID, cfg.AdminToken, os.Getenv("ASAAS_WEBHOOK_TOKEN"), checkoutLimits, anchorer, chain.AnchorMode(cfg.AnchorMode), seams)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler(pool))
	mux.Handle("/api/v1/", srv.Handler())
	// PWA da portaria (offline) — assets estáticos embutidos.
	mux.Handle("/gate/", http.StripPrefix("/gate/", http.FileServer(http.FS(gateweb.FS()))))
	mux.HandleFunc("/gate", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/gate/", http.StatusFound)
	})

	addr := ":" + cfg.Port
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	slog.Info("timbre up", "addr", addr)
	log.Fatal(httpSrv.ListenAndServe())
}

// healthHandler responde liveness + ping no banco (readiness).
func healthHandler(pool interface {
	Ping(context.Context) error
}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}
}

func logLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// seedSuperAdmin cria o primeiro super_admin do painel /admin (idempotente). É o ponto
// de entrada do operador da plataforma; sem ele, ninguém loga no /admin.
func seedSuperAdmin(ctx context.Context, pool *pgxpool.Pool, email, password string) error {
	if _, err := store.FindAdminByEmail(ctx, pool, email); err == nil {
		return nil // já existe
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	_, err = store.CreateAdmin(ctx, pool, email, hash, auth.RoleSuperAdmin)
	return err
}
