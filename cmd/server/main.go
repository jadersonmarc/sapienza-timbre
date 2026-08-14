// Command server é o binário do Timbre: sobe config, conecta ao Postgres (banco
// próprio), aplica a camada compartilhada `public`, faz catch-up das migrations de
// tenant, e serve /health + /api/v1. Data plane no espírito Control/Data da Margot,
// mas com tenancy de produtor e auth nativas.
package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jadersonmarc/sapienza-kit/tenancy"

	"github.com/jadersonmarc/sapienza-timbre/db/migrations"
	"github.com/jadersonmarc/sapienza-timbre/internal/api"
	"github.com/jadersonmarc/sapienza-timbre/internal/auth"
	"github.com/jadersonmarc/sapienza-timbre/internal/chain"
	"github.com/jadersonmarc/sapienza-timbre/internal/config"
	"github.com/jadersonmarc/sapienza-timbre/internal/dashweb"
	"github.com/jadersonmarc/sapienza-timbre/internal/db"
	"github.com/jadersonmarc/sapienza-timbre/internal/gateweb"
	"github.com/jadersonmarc/sapienza-timbre/internal/inventory"
	"github.com/jadersonmarc/sapienza-timbre/internal/ledger"
	"github.com/jadersonmarc/sapienza-timbre/internal/notify"
	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
	"github.com/jadersonmarc/sapienza-timbre/internal/producer"
	"github.com/jadersonmarc/sapienza-timbre/internal/ticketing"
	"github.com/jadersonmarc/sapienza-timbre/internal/wallet"
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

	// Gateway de pagamento: Asaas real se houver credencial, senão o Fake (default).
	var pay payment.PaymentGateway = payment.NewFakeGateway()
	payKind := "fake"
	if key := os.Getenv("ASAAS_API_KEY"); key != "" {
		pay = payment.NewAsaas(key, os.Getenv("ASAAS_BASE_URL"))
		payKind = "asaas"
	}
	// Rede: Base se configurada (RPC + contrato), senão Noop (chain desligada — a venda
	// nunca depende de rede). A emissão on-chain roda numa fila em segundo plano.
	var chainDriver chain.ChainDriver = chain.NoopChainDriver{}
	chainKind := "noop"
	if rpc := os.Getenv("CHAIN_RPC_URL"); rpc != "" {
		chainDriver = chain.NewBase(rpc, os.Getenv("CHAIN_CONTRACT"))
		chainKind = "base"
	}
	// Notificação: SMTP real se configurado (provider-agnóstico), senão o log (default).
	// Vale para a entrega do QR e para o código de acesso (OTP) do comprador.
	var notifier notify.Notifier = notify.NewLogNotifier()
	notifyKind := "log"
	smtpCfg := notify.SMTPConfig{
		Host: os.Getenv("SMTP_HOST"), Port: os.Getenv("SMTP_PORT"),
		Username: os.Getenv("SMTP_USERNAME"), Password: os.Getenv("SMTP_PASSWORD"),
		From: os.Getenv("SMTP_FROM"),
	}
	if smtpCfg.Configured() {
		notifier = notify.NewSMTPNotifier(smtpCfg)
		notifyKind = "smtp"
	}
	seams := api.Seams{
		Chain:   chainDriver,
		Payment: pay,
		Wallet:  wallet.NoopWalletProvider{},
		Notify:  notifier,
	}
	slog.Info("seams", "chain", chainKind, "chain_enabled", chainDriver.Enabled(), "payment", payKind, "wallet", "noop", "notify", notifyKind)

	// Varredura de expiração de holds (motor de reserva) por produtor, em segundo plano.
	go inventory.NewSweeper(pool).Run(ctx)
	// Worker de emissão on-chain (fila chain_jobs), em segundo plano.
	go chain.NewWorker(pool, chainDriver).Run(ctx)
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

	authz := auth.New(cfg.JWTSecret)
	prov := producer.New(pool, runner)
	srv := api.NewServer(pool, authz, prov, signer, cfg.AdminToken, os.Getenv("ASAAS_WEBHOOK_TOKEN"), seams)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler(pool))
	mux.Handle("/api/v1/", srv.Handler())
	// PWA da portaria (offline) — assets estáticos embutidos.
	mux.Handle("/gate/", http.StripPrefix("/gate/", http.FileServer(http.FS(gateweb.FS()))))
	mux.HandleFunc("/gate", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/gate/", http.StatusFound)
	})
	// Painel do produtor (tempo real, celular) — assets estáticos embutidos.
	mux.Handle("/dash/", http.StripPrefix("/dash/", http.FileServer(http.FS(dashweb.FS()))))
	mux.HandleFunc("/dash", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dash/", http.StatusFound)
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
