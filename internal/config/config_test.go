package config

import (
	"strings"
	"testing"
)

// prodEnv monta um ambiente de produção COMPLETO e seguro. Cada teste tira uma peça e
// confere que o boot é recusado — assim a trava que falta aparece pelo nome.
func prodEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TIMBRE_ENV", EnvProduction)
	t.Setenv("DATABASE_URL", "postgres://u:p@db:5432/timbre")
	t.Setenv("TIMBRE_JWT_SECRET", strings.Repeat("x", 64))
	t.Setenv("TIMBRE_TICKET_SIGNING_KEY", "c2VlZC1kZS1pbmdyZXNzbw==")
	t.Setenv("TIMBRE_ATTESTATION_KEY", "c2VlZC1kZS1hdGVzdGFkbw==")
	t.Setenv("TIMBRE_ATTESTATION_KEY_ID", "att-2026-01")
	t.Setenv("ASAAS_API_KEY", "chave-do-gateway")
	t.Setenv("ASAAS_WEBHOOK_TOKEN", "token-do-webhook")
	t.Setenv("TIMBRE_NOTIFIER", "log")
	t.Setenv("TIMBRE_ALLOW_INSECURE_DB", "")
}

func TestProductionCompleta(t *testing.T) {
	prodEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("produção completa deveria subir: %v", err)
	}
	if c.Env != EnvProduction {
		t.Fatalf("env: %s", c.Env)
	}
}

// TestProductionRecusaBuracos: cada falta é recusada NO BOOT, com o nome da variável na
// mensagem. Em dev todas têm degradado aceitável; em produção, cada uma é porta aberta.
func TestProductionRecusaBuracos(t *testing.T) {
	cases := []struct {
		name    string
		unset   string
		value   string
		esperar string
	}{
		{"webhook sem token", "ASAAS_WEBHOOK_TOKEN", "", "ASAAS_WEBHOOK_TOKEN"},
		{"gateway fake", "ASAAS_API_KEY", "", "ASAAS_API_KEY"},
		{"QR com chave efêmera", "TIMBRE_TICKET_SIGNING_KEY", "", "TIMBRE_TICKET_SIGNING_KEY"},
		{"atestado com chave efêmera", "TIMBRE_ATTESTATION_KEY", "", "TIMBRE_ATTESTATION_KEY"},
		{"segredo de sessão curto", "TIMBRE_JWT_SECRET", "curto-demais", "TIMBRE_JWT_SECRET"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prodEnv(t)
			t.Setenv(c.unset, c.value)
			_, err := Load()
			if err == nil {
				t.Fatalf("produção sem %s deveria ser recusada", c.unset)
			}
			if !strings.Contains(err.Error(), c.esperar) {
				t.Fatalf("a mensagem precisa nomear %s, veio: %v", c.esperar, err)
			}
		})
	}
}

// TestGatewayRealExigeProducao fecha o buraco de esquecer o TIMBRE_ENV: sem isto, bastaria
// não declarar produção para vender de verdade com todas as travas desligadas.
func TestGatewayRealExigeProducao(t *testing.T) {
	prodEnv(t)
	t.Setenv("TIMBRE_ENV", EnvDev)
	_, err := Load()
	if err == nil {
		t.Fatal("credencial de gateway real fora de produção deveria ser recusada")
	}
	if !strings.Contains(err.Error(), "TIMBRE_ENV=production") {
		t.Fatalf("a mensagem precisa dizer o que fazer, veio: %v", err)
	}
}

// TestDevSegueLeve: nada disso pode atrapalhar quem roda na própria máquina.
func TestDevSegueLeve(t *testing.T) {
	t.Setenv("TIMBRE_ENV", "")
	t.Setenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5433/timbre")
	t.Setenv("TIMBRE_JWT_SECRET", "segredo-de-dev")
	t.Setenv("ASAAS_API_KEY", "")
	t.Setenv("ASAAS_WEBHOOK_TOKEN", "")
	t.Setenv("TIMBRE_TICKET_SIGNING_KEY", "")
	t.Setenv("TIMBRE_ATTESTATION_KEY", "")
	t.Setenv("TIMBRE_NOTIFIER", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("dev deveria subir sem nada configurado: %v", err)
	}
	if c.Env != EnvDev {
		t.Fatalf("default deveria ser dev, veio %s", c.Env)
	}
	if c.Notifier != "log" {
		t.Fatalf("sem credencial de e-mail o provedor é log, veio %s", c.Notifier)
	}
}

// TestSSLModeDisableRecusado: pedir explicitamente conexão sem TLS em produção é decisão,
// não descuido — e precisa ser assumida por escrito.
func TestSSLModeDisableRecusado(t *testing.T) {
	prodEnv(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@db:5432/timbre?sslmode=disable")
	if _, err := Load(); err == nil {
		t.Fatal("sslmode=disable em produção deveria ser recusado")
	}
	t.Setenv("TIMBRE_ALLOW_INSECURE_DB", "true")
	if _, err := Load(); err != nil {
		t.Fatalf("com o risco assumido deveria subir: %v", err)
	}
}
