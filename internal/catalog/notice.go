package catalog

import (
	"regexp"
	"strings"
)

// Tetos de texto do produtor. PROVISÓRIOS, e cada um pelo seu motivo:
//   - aviso: cabe o recado que a pessoa lê antes de comprar, sem virar contrato escondido
//   - subtítulo: vira parágrafo e deixa de ser subtítulo; quebra o card de compartilhamento
//   - descrição: texto sem limite é página que não carrega e e-mail que não entrega
const (
	MaxNoticeLen      = 280
	MaxSubtitleLen    = 160
	MaxDescriptionLen = 5000
)

// tagLike casa qualquer coisa entre < e >. O aviso é texto de terceiro indo para página
// pública e e-mail: guardar HTML aqui seria injeção com endereço de entrega.
var tagLike = regexp.MustCompile(`(?s)<[^>]*>?`)

// controlChars remove o que não tem representação visível e serve para esconder conteúdo
// (zero-width, bidi override) ou quebrar layout.
var controlChars = regexp.MustCompile(`[\x00-\x08\x0B\x0C\x0E-\x1F\x7F\x{200B}-\x{200F}\x{202A}-\x{202E}\x{2066}-\x{2069}]`)

// SanitizeRich limpa um texto LONGO do produtor (a descrição do evento) mantendo as quebras
// de parágrafo, que é o que dá forma ao texto.
//
// Diferença para o aviso: aqui as quebras de linha SOBREVIVEM, porque é nelas que a marcação
// de lista e de parágrafo se apoia. O que não sobrevive é HTML — a formatação do produtor é
// escrita em marcação simples e renderizada por NÓS, a partir de um conjunto fechado de
// elementos. Guardar HTML de terceiro seria injeção com endereço de entrega.
func SanitizeRich(in *string, max int) *string {
	if in == nil {
		return nil
	}
	s := tagLike.ReplaceAllString(*in, "")
	s = controlChars.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	// Espaços à direita de cada linha somem, e mais de UMA linha em branco seguida colapsa
	// em uma: a linha em branco é o que separa parágrafo, e uma pilha delas é o jeito mais
	// fácil de empurrar o resto da página para baixo.
	var out []string
	blanks := 0
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			blanks++
			if blanks > 1 {
				continue
			}
		} else {
			blanks = 0
		}
		out = append(out, line)
	}
	s = strings.Trim(strings.Join(out, "\n"), "\n ")
	if s == "" {
		return nil
	}
	if len([]rune(s)) > max {
		s = strings.TrimSpace(string([]rune(s)[:max]))
	}
	return &s
}

// SanitizeNotice2 é o SanitizeNotice com teto explícito — o subtítulo é uma linha só, como
// o aviso, mas com limite próprio.
func SanitizeNotice2(in *string, max int) *string {
	out := SanitizeNotice(in)
	if out == nil {
		return nil
	}
	if len([]rune(*out)) > max {
		s := strings.TrimSpace(string([]rune(*out)[:max]))
		return &s
	}
	return out
}

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
