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
	ChainMintMode    string // off (default) — nenhum caminho materializa token
	AnchorMode       string // off (default) | log — âncora do atestado
	AttestationKey   string // seed Ed25519 base64 p/ assinar atestados (vazio = efêmera)
	AttestationKeyID string // identificador curto e estável da chave de atestação
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
		ChainMintMode:    getenv("CHAIN_MINT_MODE", "off"),
		AnchorMode:       getenv("TIMBRE_ANCHOR_MODE", "off"),
		AttestationKey:   os.Getenv("TIMBRE_ATTESTATION_KEY"),
		AttestationKeyID: os.Getenv("TIMBRE_ATTESTATION_KEY_ID"),
	}

	var missing []string
	if c.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if c.JWTSecret == "" {
		missing = append(missing, "TIMBRE_JWT_SECRET")
	}
	// Chave de atestação definida exige o identificador estável (senão a rotação invalidaria
	// atestados antigos — a verificação resolve pelo key_id, nunca pela chave corrente).
	if c.AttestationKey != "" && strings.TrimSpace(c.AttestationKeyID) == "" {
		missing = append(missing, "TIMBRE_ATTESTATION_KEY_ID (obrigatório quando TIMBRE_ATTESTATION_KEY é definida)")
	}
	if c.AnchorMode != "off" && c.AnchorMode != "log" {
		return Config{}, fmt.Errorf("TIMBRE_ANCHOR_MODE inválido: %q (use off ou log)", c.AnchorMode)
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
