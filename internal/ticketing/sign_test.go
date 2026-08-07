package ticketing

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestSignVerifyOffline é o "pronto quando" da Etapa 1.5: um token é validado por uma
// rotina que conhece APENAS a chave pública — sem banco, sem rede. Adulteração e
// chave errada falham.
func TestSignVerifyOffline(t *testing.T) {
	signer := GenerateSigner()
	seat := uuid.New()
	p := Payload{
		TicketID: uuid.New(), EventID: uuid.New(), LotID: uuid.New(), SeatID: &seat,
		FaceCents: 5000, Nonce: NewNonce(), IssuedAt: time.Now().Unix(),
	}
	token, err := signer.Token(p)
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	// Verificador só com a chave pública (a portaria offline).
	v, err := NewVerifier(signer.PublicKeyB64())
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	got, err := v.Verify(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.TicketID != p.TicketID || got.FaceCents != p.FaceCents || got.SeatID == nil || *got.SeatID != seat {
		t.Fatalf("payload divergente: %+v", got)
	}

	// Adulteração falha.
	bad := []byte(token)
	bad[len(bad)/2] ^= 0x01
	if _, err := v.Verify(string(bad)); err != ErrInvalidToken {
		t.Fatalf("token adulterado: esperava ErrInvalidToken, veio %v", err)
	}

	// Chave errada falha.
	other, _ := NewVerifier(GenerateSigner().PublicKeyB64())
	if _, err := other.Verify(token); err != ErrInvalidToken {
		t.Fatalf("chave errada: esperava ErrInvalidToken, veio %v", err)
	}
}

// TestSignerFromSeed: a mesma seed produz a mesma chave pública (QR estável).
func TestSignerFromSeed(t *testing.T) {
	seed := "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // 32 bytes base64
	a, err := NewSigner(seed)
	if err != nil {
		t.Fatalf("signer a: %v", err)
	}
	b, err := NewSigner(seed)
	if err != nil {
		t.Fatalf("signer b: %v", err)
	}
	if a.PublicKeyB64() != b.PublicKeyB64() {
		t.Fatal("mesma seed deveria dar a mesma chave pública")
	}
	// Token de a verifica com a pública de b (mesma chave).
	tok, _ := a.Token(Payload{TicketID: uuid.New(), Nonce: "n", IssuedAt: 1})
	v, _ := NewVerifier(b.PublicKeyB64())
	if _, err := v.Verify(tok); err != nil {
		t.Fatalf("verify cross-seed: %v", err)
	}
}
