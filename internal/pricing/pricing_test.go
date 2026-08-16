package pricing

import "testing"

// TestCompute cobre o modelo Sympla (§4): face limpo ao produtor; comprador paga face +
// conveniência (10% de plataforma − rebate do nível + processamento provisório = 0).
func TestCompute(t *testing.T) {
	cases := []struct {
		name                        string
		face                        int64
		method                      string
		rebatePct                   float64
		wantPlatform, wantConv, tot int64
	}{
		{"iniciante pix 10000 (rebate 10%)", 10000, MethodPix, 10, 900, 900, 10900},
		{"senior 10000 (rebate 20%)", 10000, MethodCard, 20, 800, 800, 10800},
		{"sem rebate 5000", 5000, MethodPix, 0, 500, 500, 5500},
		{"cortesia (face 0) não gera taxa", 0, MethodPix, 10, 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bd := Compute(c.face, c.method, c.rebatePct)
			if bd.PlatformFeeCents != c.wantPlatform {
				t.Fatalf("platform: quer %d, veio %d", c.wantPlatform, bd.PlatformFeeCents)
			}
			if bd.ConvenienceFeeCents != c.wantConv {
				t.Fatalf("conveniência: quer %d, veio %d", c.wantConv, bd.ConvenienceFeeCents)
			}
			if bd.TotalCents != c.tot {
				t.Fatalf("total: quer %d, veio %d", c.tot, bd.TotalCents)
			}
			if bd.FaceCents != c.face {
				t.Fatalf("face: quer %d, veio %d", c.face, bd.FaceCents)
			}
		})
	}
}
