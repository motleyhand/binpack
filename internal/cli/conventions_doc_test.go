package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/motleyhand/binpack/internal/engine"
)

// The specification, and the package the keys are declared in. Both live here
// rather than beside the engine because the engine may not touch the
// filesystem — see ADR-0008 — and because this package already reads the
// documentation tree; see diagnostics_doc_test.go and capability_doc_test.go.
const (
	conventionsSpec    = "../../docs/design/2026-08-15-architecture.md"
	conventionsHeading = "## Conventions"
	nodeKeySources     = "../../internal/engine/*.go"
	nodeKeyPrefix      = "binpack.motleyhand.com/"
)

// nodeKeys parses every non-test file in the engine package and returns the
// binpack.motleyhand.com/ keys they declare, by constant name.
//
// Parsed rather than listed. A guard written as a table of the keys a
// reviewer could think of drifts by exactly the mechanism it exists to catch:
// the commit that adds a key and forgets the document is the commit that adds
// a key and forgets the table, and the suite stays green through both. Reading
// the declarations means a new constant is in the check the moment it exists.
//
// The package rather than a named file, because a file boundary is not a
// promise the keys stay behind it. They are declared in more than one file
// already, and a guard reading one of them reads as exhaustive while missing
// whatever the others declare — silently, since a smaller map satisfies every
// check built on it. The glob follows the declarations wherever a split puts
// them, and the anchors below span two files to keep it doing so.
func nodeKeys(t *testing.T) map[string]string {
	t.Helper()

	sources, err := filepath.Glob(nodeKeySources)
	if err != nil {
		t.Fatalf("globbing %s: %v", nodeKeySources, err)
	}

	fset := token.NewFileSet()
	keys := map[string]string{}
	parsed := 0
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, source, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", source, err)
		}
		parsed++
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
	}

	// A pattern no file answers is not an error — filepath.Glob returns an
	// empty list and a nil error — so a package that moves reads as a package
	// declaring nothing, which is the vacuity a glob is otherwise vulnerable
	// to in a way a named file was not. Checked separately from the anchors
	// below so that the message names the cause rather than a symptom.
	if parsed == 0 {
		t.Fatalf("no non-test file matched %s — the package moved, and every check reading it "+
			"is now vacuous", nodeKeySources)
	}

	// The parse has to be able to fail, or every check built on it passes for
	// the wrong reason: a pass that found nothing — the package moved, the
	// block was rewritten as a var, the prefix changed — satisfies "every key
	// is documented" vacuously. Three anchors: one label and one annotation,
	// so that half a block going missing is caught, and one key that is
	// declared in a different file from the other two, so that a parse which
	// stops covering the whole package fails instead of quietly returning a
	// smaller map. All three are compiler-checked references, so a rename
	// cannot drop an anchor either.
	for name, want := range map[string]string{
		"LabelDraining":            engine.LabelDraining,
		"AnnotationSkip":           engine.AnnotationSkip,
		"NodeGroupLabelSuggestion": engine.NodeGroupLabelSuggestion,
	} {
		if keys[name] != want {
			t.Fatalf("parsing %s found %s = %q, want %q — the declarations moved, "+
				"and every check reading them is now vacuous", nodeKeySources, name, keys[name], want)
		}
	}
	return keys
}

// conventionsTable returns the rows of the Conventions table and nothing else
// in the document.
//
// Scoped to the table rather than to the whole file, or even to the section,
// because the document names most of these keys again in prose: the
// recovery-state block spells three of them out, and the paragraph directly
// under the table quotes the label inside a kubectl selector. Searched against
// the whole file, the check below passes with both table rows deleted — which
// is the only edit it exists to catch. That was measured, not reasoned about.
func conventionsTable(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(conventionsSpec)
	if err != nil {
		t.Fatalf("reading the specification: %v", err)
	}

	var rows []string
	inSection := false
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "## ") {
			inSection = line == conventionsHeading
			continue
		}
		if inSection && strings.HasPrefix(line, "|") {
			rows = append(rows, line)
		}
	}
	if len(rows) == 0 {
		t.Fatalf("no %q table in %s — the section was renamed or reformatted, and every check "+
			"reading it is now vacuous", conventionsHeading, conventionsSpec)
	}
	return strings.Join(rows, "\n")
}

func TestEveryNodeKeyBinpackDeclaresIsInTheConventions(t *testing.T) {
	// CLAUDE.md calls the architecture document the specification, and its
	// Conventions table is headed "treated as public API from the first
	// release". A key binpack puts on somebody's nodes, or tells them to put
	// there, and the spec does not promise is a promise nobody made,
	// discovered by the next maintainer.
	//
	// Declares rather than writes: NodeGroupLabelSuggestion is a key binpack
	// only ever reads, and only when the configuration names it — but it is
	// the key the documentation tells operators to apply, which puts it in a
	// GitOps repository just as surely as a key binpack wrote itself.
	table := conventionsTable(t)

	for name, key := range nodeKeys(t) {
		if !strings.Contains(table, key) {
			t.Errorf("engine.%s declares %s, which the Conventions table does not list", name, key)
		}
	}
}
