package payment

import "testing"

// TestNormalizeBaseURL: a base copiada da documentação vem em formatos diferentes, e todo
// path do cliente já começa em "/v3". Base com a versão junto gerava "/v3/v3/..." — que
// autentica e responde 404, erro que não se parece com configuração errada.
func TestNormalizeBaseURL(t *testing.T) {
	cases := map[string]string{
		"":                                 "https://api.asaas.com",
		"https://api.asaas.com":            "https://api.asaas.com",
		"https://api.asaas.com/":           "https://api.asaas.com",
		"https://api.asaas.com/v3":         "https://api.asaas.com",
		"https://api.asaas.com/v3/":        "https://api.asaas.com",
		"https://api-sandbox.asaas.com/v3": "https://api-sandbox.asaas.com",
		"https://sandbox.asaas.com/api/v3": "https://sandbox.asaas.com/api",
		"https://sandbox.asaas.com/api":    "https://sandbox.asaas.com/api",
	}
	for in, want := range cases {
		if got := NewAsaas("k", in).BaseURL(); got != want {
			t.Errorf("base %q → %q, esperava %q", in, got, want)
		}
	}
}
