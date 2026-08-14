package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// BuyerAudience marca o token do COMPRADOR — escopo próprio, distinto do de produtor/
// colaborador. Rota de produtor rejeita token com esta audience; rota de comprador exige
// ela. Assim um token nunca vale na superfície do outro (§4.1).
const BuyerAudience = "buyer"

// buyerTTL é a validade do token do comprador. PROVISÓRIO — curto; sem refresh ainda, o
// comprador refaz o OTP quando expira.
const buyerTTL = 24 * time.Hour

// IssueBuyer assina um token de comprador para o subject (a pessoa em public.subjects).
func (a *Authenticator) IssueBuyer(subjectID uuid.UUID) (string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Subject:   subjectID.String(),
		Issuer:    Issuer,
		Audience:  jwt.ClaimStrings{BuyerAudience},
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(buyerTTL)),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(a.secret)
}

// VerifyBuyer valida o token do comprador (assinatura HS256, issuer e audience "buyer") e
// devolve o subject id. Token de produtor (sem essa audience) é recusado aqui.
func (a *Authenticator) VerifyBuyer(tokenString string) (uuid.UUID, error) {
	claims := &jwt.RegisteredClaims{}
	_, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("método de assinatura inesperado: %v", t.Header["alg"])
		}
		return a.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(Issuer), jwt.WithAudience(BuyerAudience))
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.Parse(claims.Subject)
}
