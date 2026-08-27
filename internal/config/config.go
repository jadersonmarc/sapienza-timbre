// Package config carrega a configuração do Timbre a partir do ambiente. Sem viper
// e sem arquivo: um struct simples com Load() que falha cedo quando falta algo
// obrigatório. Convenção de nomes: prefixo TIMBRE_ para segredos do produto.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Ambientes reconhecidos. O default é dev de propósito: um ambiente novo nasce sem poder
// mexer em dinheiro real, e ligar isso tem de ser um ato deliberado.
const (
	EnvDev        = "dev"
	EnvProduction = "production"
)

// minJWTSecretLen é o tamanho mínimo do segredo que assina a sessão. Um segredo curto é
// atacável por força bruta offline a partir de UM token capturado, e um token forjado vale
// como sessão de owner. `openssl rand -base64 48` produz 64 caracteres.
const minJWTSecretLen = 32

// Config é a configuração resolvida do processo.
type Config struct {
	Env              string // dev (default) | production — endurece as exigências
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
	Notifier         string // log (default) | resend — provedor de e-mail
	ResendAPIKey     string // chave da API do Resend (obrigatória quando Notifier=resend)
	MailFrom         string // remetente "Nome <email>" (domínio do envio, nunca fixo no código)
	MailReplyTo      string // opcional — reply-to das mensagens
	PublicBaseURL    string // base pública do site p/ montar links nas mensagens

	// Gateway de pagamento. Ficavam soltos em os.Getenv no boot, fora de qualquer
	// validação — e é justamente o par em que faltar um valor custa dinheiro.
	AsaasAPIKey       string // vazio = FakeGateway (proibido em produção)
	AsaasBaseURL      string // vazio = produção do gateway
	AsaasWebhookToken string // obrigatório em produção: sem ele o webhook é público

	// AllowInsecureDB deixa a produção subir com conexão de banco SEM TLS. Existe porque a
	// correção é no servidor de banco, não aqui — mas sem uma trava explícita ninguém
	// descobre que credenciais e CPF trafegam em claro.
	AllowInsecureDB bool
}

// Load lê o ambiente e valida os campos obrigatórios. Retorna erro em vez de
// abortar para o main decidir como reportar.
func Load() (Config, error) {
	c := Config{
		Env:              getenv("TIMBRE_ENV", EnvDev),
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
		// Aceita também os nomes usados pelo console (RESEND_API_KEY/MAIL_FROM): é o mesmo
		// provedor e a mesma conta, e nome divergente já custou um envio silenciosamente
		// desligado em produção. O nome com prefixo TIMBRE_ tem prioridade.
		Notifier:      os.Getenv("TIMBRE_NOTIFIER"),
		ResendAPIKey:  firstSet("TIMBRE_RESEND_API_KEY", "RESEND_API_KEY"),
		MailFrom:      firstSet("TIMBRE_MAIL_FROM", "MAIL_FROM"),
		MailReplyTo:   firstSet("TIMBRE_MAIL_REPLY_TO", "MAIL_REPLY_TO"),
		PublicBaseURL: os.Getenv("TIMBRE_PUBLIC_BASE_URL"),

		AsaasAPIKey:       os.Getenv("ASAAS_API_KEY"),
		AsaasBaseURL:      os.Getenv("ASAAS_BASE_URL"),
		AsaasWebhookToken: os.Getenv("ASAAS_WEBHOOK_TOKEN"),
		AllowInsecureDB:   os.Getenv("TIMBRE_ALLOW_INSECURE_DB") == "true",
	}

	if c.Env != EnvDev && c.Env != EnvProduction {
		return Config{}, fmt.Errorf("TIMBRE_ENV inválido: %q (use dev ou production)", c.Env)
	}
	// Credencial de gateway REAL fora de produção é contradição, e é a forma mais provável
	// de as travas abaixo serem contornadas sem ninguém perceber: bastaria esquecer o
	// TIMBRE_ENV para vender de verdade sem nenhuma delas valendo.
	if c.AsaasAPIKey != "" && c.Env != EnvProduction {
		return Config{}, fmt.Errorf("ASAAS_API_KEY definida com TIMBRE_ENV=%s: dinheiro real exige TIMBRE_ENV=production", c.Env)
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
	// Sem TIMBRE_NOTIFIER explícito, o provedor é DEDUZIDO: havendo chave e remetente,
	// envia de verdade (é o comportamento do mailer do console). Só cai em 'log' quando
	// não há como enviar — assim configurar a chave basta, sem um switch para esquecer.
	if c.Notifier == "" {
		if c.ResendAPIKey != "" && c.MailFrom != "" {
			c.Notifier = "resend"
		} else {
			c.Notifier = "log"
		}
	}
	// Notifier: log | resend. Valor desconhecido falha — nada de default silencioso.
	switch c.Notifier {
	case "log":
	case "resend":
		if c.ResendAPIKey == "" {
			return Config{}, fmt.Errorf("TIMBRE_NOTIFIER=resend exige TIMBRE_RESEND_API_KEY")
		}
		if c.MailFrom == "" {
			return Config{}, fmt.Errorf("TIMBRE_NOTIFIER=resend exige TIMBRE_MAIL_FROM")
		}
	default:
		return Config{}, fmt.Errorf("TIMBRE_NOTIFIER inválido: %q (use log ou resend)", c.Notifier)
	}
	if c.Env == EnvProduction {
		missing = append(missing, c.productionGaps()...)
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("faltam variáveis obrigatórias: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

// productionGaps lista o que, faltando, torna a operação insegura de verdade. Em dev cada
// um destes tem um degradado aceitável (chave efêmera, gateway fake, webhook aberto na sua
// máquina). Em produção, o mesmo degradado é uma porta aberta — e cada item aqui já foi um
// incidente esperando acontecer, não hipótese.
func (c Config) productionGaps() []string {
	var gaps []string
	// Webhook sem token é PÚBLICO: qualquer um que saiba a URL confirma um pedido que não
	// pagou e recebe ingresso válido, ou manda estornar e queima o ingresso de outro.
	if c.AsaasWebhookToken == "" {
		gaps = append(gaps, "ASAAS_WEBHOOK_TOKEN (sem ele o webhook aceita qualquer origem: confirma venda não paga e queima ingresso alheio)")
	}
	// Gateway fake em produção emite ingresso sem cobrar nada.
	if c.AsaasAPIKey == "" {
		gaps = append(gaps, "ASAAS_API_KEY (sem ela o gateway é o Fake, que emite ingresso sem cobrar)")
	}
	// Chave efêmera invalida todo QR já emitido a cada restart, e a portaria offline
	// embarca a chave pública — ela deixa de reconhecer os ingressos vendidos.
	if c.TicketSigningKey == "" {
		gaps = append(gaps, "TIMBRE_TICKET_SIGNING_KEY (chave efêmera invalida todo QR emitido a cada restart)")
	}
	// Atestado assinado por chave efêmera não é verificável depois do restart: a
	// comprovação de público deixa de provar.
	if c.AttestationKey == "" {
		gaps = append(gaps, "TIMBRE_ATTESTATION_KEY (atestado assinado por chave efêmera deixa de ser verificável)")
	}
	if len(c.JWTSecret) < minJWTSecretLen {
		gaps = append(gaps, fmt.Sprintf(
			"TIMBRE_JWT_SECRET com pelo menos %d caracteres (tem %d — um token capturado permite quebrar o segredo offline e forjar sessão de owner; gere com openssl rand -base64 48)",
			minJWTSecretLen, len(c.JWTSecret)))
	}
	// sslmode=disable é a única forma de recusar TLS pela string. A ausência de TLS no
	// servidor só se descobre conectando — a checagem está no boot.
	if !c.AllowInsecureDB && strings.Contains(c.DatabaseURL, "sslmode=disable") {
		gaps = append(gaps, "DATABASE_URL sem sslmode=disable (ou TIMBRE_ALLOW_INSECURE_DB=true, assumindo o risco)")
	}
	return gaps
}

// firstSet devolve o valor da primeira variável definida, na ordem dada.
func firstSet(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
