package payout_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// termosExtintos são conceitos REMOVIDOS por decisão, não pendências. Cada um já voltou uma
// vez por reintrodução silenciosa em algum caminho lateral, e é por isso que existe um teste
// e não só um comentário.
//
// A busca é por palavra inteira: `strings.Split` e `net.SplitHostPort` não são split de
// pagamento, e um termo genérico demais transformaria a guarda em ruído que alguém desliga.
var termosExtintos = []*regexp.Regexp{
	// Divisão da cobrança entre recebedores no ato da venda.
	regexp.MustCompile(`\bsplit_transfers\b`),
	regexp.MustCompile(`\bsplit_refunds\b`),
	regexp.MustCompile(`\bSplitItem\b`),
	regexp.MustCompile(`\bMarkSplitStatus\b`),
	regexp.MustCompile(`"splits"`),
	// Conta do produtor no gateway.
	regexp.MustCompile(`\bsubaccount\b`),
	regexp.MustCompile(`\bSubaccounts?\b`),
	regexp.MustCompile(`\basaas_wallet_id\b`),
	regexp.MustCompile(`\bproducer_asaas_accounts\b`),
	regexp.MustCompile(`/v3/accounts`),
	// Programa de níveis e rebate.
	regexp.MustCompile(`\bprogram\.Apurar\b`),
	regexp.MustCompile(`\bplatform_fee_rules\b`),
	regexp.MustCompile(`\bproducer_tier_history\b`),
	// Saldo devedor do produtor: o cenário "já sacou" deixou de existir.
	regexp.MustCompile(`\bplatform_covered\b`),
}

// TestNadaDeSplitSubcontaOuNivelNoCodigo varre o CÓDIGO atrás dos conceitos extintos.
//
// Só código Go: as migrations são forward-only e nomeiam tanto o que criaram quanto o que
// derrubaram — é a história, e reescrevê-la seria pior. O que este teste barra é um caminho
// VIVO: código que ainda lê, escreve ou fala com eles.
func TestNadaDeSplitSubcontaOuNivelNoCodigo(t *testing.T) {
	raiz := "../.."
	excecoes := map[string]bool{
		// Este arquivo é a própria guarda.
		"internal/payout/legacy_test.go": true,
		// E este afirma a AUSÊNCIA do campo no payload da cobrança: citar o nome é o teste.
		"internal/payment/charge_test.go": true,
	}
	var achados []string
	err := filepath.WalkDir(raiz, func(caminho string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", ".git", "node_modules", "db":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(caminho) != ".go" {
			return nil
		}
		rel, _ := filepath.Rel(raiz, caminho)
		rel = filepath.ToSlash(rel)
		if excecoes[rel] {
			return nil
		}
		raw, err := os.ReadFile(caminho)
		if err != nil {
			return err
		}
		for i, linha := range strings.Split(string(raw), "\n") {
			for _, re := range termosExtintos {
				if re.MatchString(linha) {
					achados = append(achados, rel+":"+itoa(i+1)+": "+strings.TrimSpace(linha))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("varrer o repositório: %v", err)
	}
	if len(achados) > 0 {
		t.Fatalf("conceito extinto de volta no código:\n  %s", strings.Join(achados, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
