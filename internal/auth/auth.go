// Package auth é a autenticação NATIVA do Timbre: emite e valida o próprio JWT
// (HS256, issuer "sapienza-timbre") para colaboradores de produtor, com permissões
// granulares nas claims. Difere da Margot, que apenas valida o JWT do core — aqui o
// produtor é criado e autenticado dentro do Timbre, com papéis próprios do produto.
package auth

import (
	"fmt"
	"slices"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Issuer identifica quem emitiu o token (nós mesmos).
const Issuer = "sapienza-timbre"

// defaultTTL é a validade do token de sessão. Sem refresh nesta etapa, então é uma
// janela de trabalho generosa; encurtar quando houver refresh.
const defaultTTL = 12 * time.Hour

// AllPermissions são as permissões granulares por colaborador (a granularidade
// exigida pela Etapa 1.1). O owner tem todas implicitamente.
var AllPermissions = []string{"checkin", "financeiro", "relatorios", "atendimento"}

// ValidPermission diz se p é uma permissão conhecida.
func ValidPermission(p string) bool {
	return slices.Contains(AllPermissions, p)
}

// Claims são as claims do token do Timbre. Subject = collaborator id.
type Claims struct {
	ProducerID  uuid.UUID `json:"pid"`
	Permissions []string  `json:"perms"`
	Owner       bool      `json:"owner"`
	SessionVer  int       `json:"sver"`
	jwt.RegisteredClaims
}

// CollaboratorID faz o parse do Subject.
func (c *Claims) CollaboratorID() (uuid.UUID, error) {
	return uuid.Parse(c.Subject)
}

// Has diz se o token carrega a permissão (owner passa sempre).
func (c *Claims) Has(permission string) bool {
	if c.Owner {
		return true
	}
	return slices.Contains(c.Permissions, permission)
}

// Identity é o que emitimos num token.
type Identity struct {
	CollaboratorID uuid.UUID
	ProducerID     uuid.UUID
	Owner          bool
	SessionVersion int
	Permissions    []string
}

// Authenticator emite e valida tokens com um segredo HS256.
type Authenticator struct {
	secret []byte
	ttl    time.Duration
}

// New constrói o autenticador a partir do segredo (TIMBRE_JWT_SECRET).
func New(secret string) *Authenticator {
	return &Authenticator{secret: []byte(secret), ttl: defaultTTL}
}

// Issue assina um token para a identidade.
func (a *Authenticator) Issue(id Identity) (string, error) {
	now := time.Now()
	claims := Claims{
		ProducerID:  id.ProducerID,
		Permissions: id.Permissions,
		Owner:       id.Owner,
		SessionVer:  id.SessionVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.CollaboratorID.String(),
			Issuer:    Issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(a.ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(a.secret)
}

// Verify valida assinatura, método (só HS256), expiração e issuer.
func (a *Authenticator) Verify(tokenString string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("método de assinatura inesperado: %v", t.Header["alg"])
		}
		return a.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(Issuer))
	if err != nil {
		return nil, err
	}
	return claims, nil
}
