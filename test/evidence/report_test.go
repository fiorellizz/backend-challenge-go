//go:build evidence

package evidence

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// report accumulates the sections of EVIDENCE.md while the scenarios run
// and writes the file when the package finishes.
var report = &reportBuilder{}

type reportBuilder struct {
	mu       sync.Mutex
	sections []string
}

// section records one scenario: a title, what was done, and the facts
// observed. Facts are rendered as a table; the last row states the verdict.
func (r *reportBuilder) section(title, setup string, facts [][2]string, verdict string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n%s\n\n| Observação | Valor |\n|---|---|\n", title, setup)
	for _, f := range facts {
		fmt.Fprintf(&b, "| %s | %s |\n", f[0], f[1])
	}
	fmt.Fprintf(&b, "\n**Resultado:** %s\n", verdict)
	r.sections = append(r.sections, b.String())
}

func (r *reportBuilder) write(path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	b.WriteString("# Evidências de execução distribuída\n\n")
	fmt.Fprintf(&b, "Gerado por `make evidence` em %s contra três instâncias reais da aplicação ", time.Now().UTC().Format(time.RFC3339))
	b.WriteString("(`app-1`, `app-2`, `app-3` do Docker Compose), cada uma com seu próprio processo, pool de conexões e memória, ")
	b.WriteString("compartilhando PostgreSQL, LocalStack (SQS) e Keycloak. Os cenários abaixo correspondem ao item 13 do desafio.\n\n")
	b.WriteString("Cada seção descreve o que foi feito, o que foi observado no banco, nas filas e na API, e o resultado. ")
	b.WriteString("Os identificadores externos recebem um prefixo aleatório por execução, então o arquivo pode ser regenerado a qualquer momento.\n\n")
	for _, s := range r.sections {
		b.WriteString(s)
		b.WriteString("\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func TestMain(m *testing.M) {
	code := m.Run()
	if os.Getenv("EVIDENCE") != "" && len(report.sections) > 0 {
		path := os.Getenv("EVIDENCE_FILE")
		if path == "" {
			path = "../../EVIDENCE.md"
		}
		if err := report.write(path); err != nil {
			fmt.Fprintln(os.Stderr, "write evidence:", err)
			code = 1
		} else {
			fmt.Println(">> EVIDENCE.md escrito em", path)
		}
	}
	os.Exit(code)
}

func verdict(ok bool) string {
	if ok {
		return "✅ garantido"
	}
	return "❌ violado"
}
