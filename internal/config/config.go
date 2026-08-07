// Package config carrega a configuração do Timbre a partir do ambiente. Sem viper
// e sem arquivo: um struct simples com Load() que falha cedo quando falta algo
// obrigatório. Convenção de nomes: prefixo TIMBRE_ para segredos do produto.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Config é a configuração resolvida do processo.
type Config struct {
	DatabaseURL      string // obrigatório
	Port             string // default 8082
	JWTSecret        string // obrigatório — assina/valida o JWT nativo do Timbre
	AdminToken       string // bootstrap: cria produtor (aprovação de produtor vem na 1.7)
	EncKey           string // reservado (AES-256-GCM por-tenant); vazio nesta etapa
	TicketSigningKey string // seed Ed25519 base64 p/ assinar ingressos (vazio = efêmera)
	LogLevel         string // debug|info|warn|error (default info)
}

// Load lê o ambiente e valida os campos obrigatórios. Retorna erro em vez de
// abortar para o main decidir como reportar.
func Load() (Config, error) {
	c := Config{
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		Port:             getenv("PORT", "8082"),
		JWTSecret:        os.Getenv("TIMBRE_JWT_SECRET"),
		AdminToken:       os.Getenv("TIMBRE_ADMIN_TOKEN"),
		EncKey:           os.Getenv("TIMBRE_ENC_KEY"),
		TicketSigningKey: os.Getenv("TIMBRE_TICKET_SIGNING_KEY"),
		LogLevel:         getenv("LOG_LEVEL", "info"),
	}

	var missing []string
	if c.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if c.JWTSecret == "" {
		missing = append(missing, "TIMBRE_JWT_SECRET")
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("faltam variáveis obrigatórias: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
