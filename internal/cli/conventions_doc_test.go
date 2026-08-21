package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/motleyhand/binpack/internal/engine"
)

// The specification, and the file the keys are declared in. Both live here
// rather than beside the engine because the engine may not touch the
// filesystem — see ADR-0008 — and because this package already reads the
// documentation tree; see diagnostics_doc_test.go and capability_doc_test.go.
const (
	conventionsSpec = "../../docs/design/2026-08-15-architecture.md"
	nodeKeySource   = "../../internal/engine/decide.go"
	nodeKeyPrefix   = "binpack.motleyhand.com/"
)

// nodeKeys parses the engine's constant block and returns every declared
// binpack.motleyhand.com/ key, by constant name.
//
// Parsed rather than listed. A guard written as a table of the keys a
// reviewer could think of drifts by exactly the mechanism it exists to catch:
// the commit that adds a key and forgets the document is the commit that adds
// a key and forgets the table, and the suite stays green through both. Reading
// the declarations means a new constant is in the check the moment it exists.
func nodeKeys(t *testing.T) map[string]string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), nodeKeySource, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", nodeKeySource, err)
	}

	keys := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != len(spec.Values) {
			return true
		}
		for i, name := range spec.Names {
			lit, ok := spec.Values[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.HasPrefix(value, nodeKeyPrefix) {
				continue
			}
			keys[name.Name] = value
		}
		return true
	})

	// The parse has to be able to fail, or every check built on it passes for
	// the wrong reason: a pass that found nothing — the file moved, the block
	// was rewritten as a var, the prefix changed — satisfies "every key is
	// documented" vacuously. Two anchors, one label and one annotation, so
	// that half a block going missing is caught too. Both are compiler-checked
	// references, so a rename cannot quietly drop an anchor either.
	for name, want := range map[string]string{
		"LabelDraining":  engine.LabelDraining,
		"AnnotationSkip": engine.AnnotationSkip,
	} {
		if keys[name] != want {
			t.Fatalf("parsing %s found %s = %q, want %q — the declarations moved, "+
				"and every check reading them is now vacuous", nodeKeySource, name, keys[name], want)
		}
	}
	return keys
}

func TestEveryNodeKeyBinpackWritesIsInTheConventions(t *testing.T) {
	// CLAUDE.md calls the architecture document the specification, and its
	// Conventions table is headed "treated as public API from the first
	// release". A key binpack writes on somebody's nodes and the spec does not
	// mention is a promise nobody made, discovered by the next maintainer.
	data, err := os.ReadFile(conventionsSpec)
	if err != nil {
		t.Fatalf("reading the specification: %v", err)
	}
	doc := string(data)

	for name, key := range nodeKeys(t) {
		if !strings.Contains(doc, key) {
			t.Errorf("engine.%s writes %s, which the Conventions table does not list", name, key)
		}
	}
}
