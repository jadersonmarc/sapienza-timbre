package catalog

import "testing"

// TestSanitizeNotice: o aviso é texto de terceiro indo para página pública e e-mail. Guardar
// HTML aqui seria injeção com endereço de entrega — a limpeza é na ESCRITA, para o que está
// no banco já ser seguro em qualquer superfície.
func TestSanitizeNotice(t *testing.T) {
	casos := []struct {
		nome string
		in   string
		want string
	}{
		{"tag simples", `Chegue cedo <b>mesmo</b>`, "Chegue cedo mesmo"},
		{"script", `Ok<script>alert(1)</script>`, "Okalert(1)"},
		{"tag aberta sem fechar", `Aviso <img src=x onerror=alert(1)`, "Aviso"},
		{"quebra de linha vira espaço", "Linha um\nLinha dois", "Linha um Linha dois"},
		{"espaços colapsam", "  muito    espaço  ", "muito espaço"},
		{"zero-width escondendo texto", "Normal​invisível", "Normalinvisível"},
		{"bidi override", "texto‮odissergni", "textoodissergni"},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			got := SanitizeNotice(&c.in)
			if got == nil || *got != c.want {
				t.Fatalf("esperava %q, veio %v", c.want, derefOrNil(got))
			}
		})
	}
}

// TestSanitizeNoticeVazio: aviso em branco não é aviso — vira nulo, e a tela não abre um
// bloco vazio para ninguém.
func TestSanitizeNoticeVazio(t *testing.T) {
	for _, in := range []string{"", "   ", "<b></b>", "\n\n"} {
		if got := SanitizeNotice(&in); got != nil {
			t.Fatalf("%q deveria virar nulo, veio %q", in, *got)
		}
	}
	if SanitizeNotice(nil) != nil {
		t.Fatal("nulo continua nulo")
	}
}

// TestSanitizeNoticeTeto: o aviso cabe no card e no e-mail; passar disso vira contrato
// escondido em letra miúda.
func TestSanitizeNoticeTeto(t *testing.T) {
	longo := ""
	for range MaxNoticeLen + 50 {
		longo += "á"
	}
	got := SanitizeNotice(&longo)
	if got == nil || len([]rune(*got)) != MaxNoticeLen {
		t.Fatalf("esperava corte em %d runas, veio %d", MaxNoticeLen, len([]rune(*got)))
	}
}

func derefOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
