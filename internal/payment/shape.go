package payment

import (
	"encoding/json"
	"sort"
	"strings"
)

// maxShapePaths limita o que uma linha de log despeja. Um payload inesperadamente grande não
// pode virar log de megabytes.
const maxShapePaths = 60

// jsonShape devolve os CAMINHOS de chave de um JSON, sem os valores.
//
// Serve para responder, olhando o log de uma operação real, perguntas de forma — "o aviso de
// estorno traz o id do estorno ou só o da cobrança?" — sem guardar nada do conteúdo. Valor de
// webhook de pagamento carrega dado do comprador; a estrutura, não.
func jsonShape(raw []byte) []string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return []string{"<json ilegível>"}
	}
	set := map[string]bool{}
	walk("", v, set)
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) > maxShapePaths {
		out = append(out[:maxShapePaths:maxShapePaths], "…")
	}
	return out
}

func walk(prefix string, v any, set map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			set[path] = true
			walk(path, child, set)
		}
	case []any:
		// Índice não interessa: o que importa é a forma dos itens, não quantos são.
		for _, child := range t {
			walk(prefix+"[]", child, set)
		}
	}
}

// ShapeOf é a forma de um JSON como uma linha só, para log.
func ShapeOf(raw []byte) string { return strings.Join(jsonShape(raw), " ") }
