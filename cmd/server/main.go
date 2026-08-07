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
	"github.com/jadersonmarc/sapienza-timbre/internal/db"
	"github.com/jadersonmarc/sapienza-timbre/internal/inventory"
	"github.com/jadersonmarc/sapienza-timbre/internal/notify"
	"github.com/jadersonmarc/sapienza-timbre/internal/payment"
	"github.com/jadersonmarc/sapienza-timbre/internal/producer"
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
	seams := api.Seams{
		Chain:   chain.NoopChainDriver{},
		Payment: pay,
		Wallet:  wallet.NoopWalletProvider{},
		Notify:  notify.NewLogNotifier(),
	}
	slog.Info("seams", "chain", "noop", "payment", payKind, "wallet", "noop", "notify", "log")

	// Varredura de expiração de holds (motor de reserva) por produtor, em segundo plano.
	go inventory.NewSweeper(pool).Run(ctx)

	authz := auth.New(cfg.JWTSecret)
	prov := producer.New(pool, runner)
	srv := api.NewServer(pool, authz, prov, cfg.AdminToken, os.Getenv("ASAAS_WEBHOOK_TOKEN"), seams)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler(pool))
	mux.Handle("/api/v1/", srv.Handler())

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
