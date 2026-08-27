package catalog

import (
	"regexp"
	"strings"
)

// MaxNoticeLen é o teto do aviso do produtor. PROVISÓRIO: 280 caracteres — cabe o recado
// que a pessoa lê antes de comprar e não vira contrato escondido no card.
const MaxNoticeLen = 280

// tagLike casa qualquer coisa entre < e >. O aviso é texto de terceiro indo para página
// pública e e-mail: guardar HTML aqui seria injeção com endereço de entrega.
var tagLike = regexp.MustCompile(`(?s)<[^>]*>?`)

// controlChars remove o que não tem representação visível e serve para esconder conteúdo
// (zero-width, bidi override) ou quebrar layout.
var controlChars = regexp.MustCompile(`[\x00-\x08\x0B\x0C\x0E-\x1F\x7F\x{200B}-\x{200F}\x{202A}-\x{202E}\x{2066}-\x{2069}]`)

// SanitizeNotice devolve o aviso como TEXTO PURO, dentro do teto.
//
// A limpeza é na ESCRITA, não na leitura: assim o que está no banco já é seguro para
// qualquer superfície — a página, o e-mail, um relatório futuro —, em vez de depender de
// cada leitor lembrar de escapar. Vazio vira nulo: aviso em branco não é aviso.
func SanitizeNotice(in *string) *string {
	if in == nil {
		return nil
	}
	s := tagLike.ReplaceAllString(*in, "")
	s = controlChars.ReplaceAllString(s, "")
	// Quebra de linha vira espaço: o card e o e-mail são de uma linha só, e o produtor
	// colando um parágrafo não pode desmontar o layout de quem compra.
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return nil
	}
	if len([]rune(s)) > MaxNoticeLen {
		s = string([]rune(s)[:MaxNoticeLen])
	}
	return &s
}
