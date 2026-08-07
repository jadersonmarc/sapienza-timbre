package auth

import "golang.org/x/crypto/bcrypt"

// HashPassword gera um hash bcrypt (custo default) da senha em claro.
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ComparePassword confere a senha contra o hash (tempo constante do bcrypt).
func ComparePassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
