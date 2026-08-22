// Package ticketing assina e verifica ingressos com Ed25519. O ponto central da
// Etapa 1.5: o QR é um token AUTOCONTIDO (payload + assinatura) que a portaria valida
// OFFLINE, conhecendo apenas a chave pública — sem banco e sem rede no portão.
package ticketing

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ErrInvalidToken: token malformado ou assinatura inválida.
var ErrInvalidToken = errors.New("ticketing: token inválido")

// Payload é o conteúdo assinado do ingresso. SÓ identificadores e valor de face —
// nenhum dado pessoal (guardrail: nada pessoal vai para a rede/QR). A ordem dos
// campos é fixa para o JSON ser determinístico (reassinável a partir da linha).
type Payload struct {
	TicketID  uuid.UUID  `json:"tid"`
	EventID   uuid.UUID  `json:"eid"`
	LotID     uuid.UUID  `json:"lid"`
	SeatID    *uuid.UUID `json:"sid,omitempty"`
	FaceCents int64      `json:"face"`
	Nonce     string     `json:"n"`
	IssuedAt  int64      `json:"iat"`
}

var b64 = base64.RawURLEncoding

// Signer assina payloads com a chave privada Ed25519 da plataforma.
type Signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// NewSigner constrói o Signer a partir de uma seed de 32 bytes em base64.
func NewSigner(seedB64 string) (*Signer, error) {
	seed, err := decodeKey(seedB64)
	if err != nil {
		return nil, fmt.Errorf("seed inválida: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("seed deve ter %d bytes", ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &Signer{priv: priv, pub: priv.Public().(ed25519.PublicKey)}, nil
}

// GenerateSigner cria um Signer com chave aleatória (dev/testes). Em produção use uma
// seed persistente (TIMBRE_TICKET_SIGNING_KEY), senão o QR muda a cada restart.
func GenerateSigner() *Signer {
	seed := make([]byte, ed25519.SeedSize)
	_, _ = rand.Read(seed)
	priv := ed25519.NewKeyFromSeed(seed)
	return &Signer{priv: priv, pub: priv.Public().(ed25519.PublicKey)}
}

// PublicKeyB64 é a chave pública em base64 — é ela que a portaria embarca.
func (s *Signer) PublicKeyB64() string { return b64.EncodeToString(s.pub) }

// Verifier devolve um verificador com a chave pública deste signer (validação
// offline sem expor a privada).
func (s *Signer) Verifier() *Verifier { return &Verifier{pub: s.pub} }

// Token assina o payload e devolve o token compacto "b64(payload).b64(assinatura)".
func (s *Signer) Token(p Payload) (string, error) {
	msg, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(s.priv, msg)
	return b64.EncodeToString(msg) + "." + b64.EncodeToString(sig), nil
}

// Sign devolve só a assinatura de um payload (para gravar em tickets.signature).
func (s *Signer) Sign(p Payload) ([]byte, error) {
	msg, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(s.priv, msg), nil
}

// SignBytes assina bytes arbitrários (ex.: o resumo SHA-256 do atestado). Usado com a
// chave de ATESTAÇÃO (propósito distinto da chave do QR).
func (s *Signer) SignBytes(b []byte) []byte {
	return ed25519.Sign(s.priv, b)
}

// PublicKeyBytes devolve a chave pública crua (para a verificação pública do atestado).
func (s *Signer) PublicKeyBytes() []byte {
	return append([]byte(nil), s.pub...)
}

// Verifier valida tokens conhecendo APENAS a chave pública. É a portaria offline.
type Verifier struct {
	pub ed25519.PublicKey
}

// NewVerifier constrói o verificador a partir da chave pública em base64.
func NewVerifier(pubB64 string) (*Verifier, error) {
	pub, err := decodeKey(pubB64)
	if err != nil {
		return nil, fmt.Errorf("chave pública inválida: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("chave pública deve ter %d bytes", ed25519.PublicKeySize)
	}
	return &Verifier{pub: ed25519.PublicKey(pub)}, nil
}

// Verify checa a assinatura e devolve o payload. Não toca em banco nem em rede.
func (v *Verifier) Verify(token string) (Payload, error) {
	msgB64, sigB64, ok := strings.Cut(token, ".")
	if !ok {
		return Payload{}, ErrInvalidToken
	}
	msg, err := b64.DecodeString(msgB64)
	if err != nil {
		return Payload{}, ErrInvalidToken
	}
	sig, err := b64.DecodeString(sigB64)
	if err != nil {
		return Payload{}, ErrInvalidToken
	}
	if !ed25519.Verify(v.pub, msg, sig) {
		return Payload{}, ErrInvalidToken
	}
	var p Payload
	if err := json.Unmarshal(msg, &p); err != nil {
		return Payload{}, ErrInvalidToken
	}
	return p, nil
}

// NewNonce gera um nonce curto para o payload.
func NewNonce() string {
	buf := make([]byte, 12)
	_, _ = rand.Read(buf)
	return b64.EncodeToString(buf)
}

// decodeKey aceita base64 padrão ou raw-url.
func decodeKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}
