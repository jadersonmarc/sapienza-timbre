package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// AdminAudience marca o token do ADMIN de plataforma (painel /admin). Escopo próprio,
// distinto do colaborador e do comprador: um token de admin não vale em rota de
// produtor nem de comprador, e vice-versa (§4.1).
const AdminAudience = "admin"

// adminTTL é a validade do token de admin. Igual ao de colaborador (sem refresh ainda).
const adminTTL = 12 * time.Hour

// Roles de admin. super_admin tem acesso total (inclusive gerir outros admins);
// admin opera a plataforma sem tocar na gestão de admins.
const (
	RoleAdmin      = "admin"
	RoleSuperAdmin = "super_admin"
)

// AdminClaims são as claims do token de admin. Subject = admin id.
type AdminClaims struct {
	Role       string `json:"role"`
	SessionVer int    `json:"sver"`
	jwt.RegisteredClaims
}

// AdminID faz o parse do Subject.
func (c *AdminClaims) AdminID() (uuid.UUID, error) {
	return uuid.Parse(c.Subject)
}

// IsSuperAdmin diz se o admin tem acesso total.
func (c *AdminClaims) IsSuperAdmin() bool {
	return c.Role == RoleSuperAdmin
}

// IssueAdmin assina um token de admin (escopo "admin").
func (a *Authenticator) IssueAdmin(adminID uuid.UUID, role string, sessionVersion int) (string, error) {
	now := time.Now()
	claims := AdminClaims{
		Role:       role,
		SessionVer: sessionVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   adminID.String(),
			Issuer:    Issuer,
			Audience:  jwt.ClaimStrings{AdminAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(adminTTL)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(a.secret)
}

// VerifyAdmin valida o token de admin (assinatura HS256, issuer e audience "admin").
// Tokens de colaborador/comprador (sem essa audience) são recusados aqui.
func (a *Authenticator) VerifyAdmin(tokenString string) (*AdminClaims, error) {
	claims := &AdminClaims{}
	_, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("método de assinatura inesperado: %v", t.Header["alg"])
		}
		return a.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(Issuer), jwt.WithAudience(AdminAudience))
	if err != nil {
		return nil, err
	}
	return claims, nil
}
