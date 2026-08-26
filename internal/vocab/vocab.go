// Package vocab reads the string constants a package declares.
//
// binpack publishes several closed vocabularies — skip codes, verdicts,
// decision codes, abandonment reasons, Event reasons and actions — and each
// has an enumerator beside it that a reference page and a metric's
// pre-initialised series are checked against. Every one of those checks visits
// what the enumerator holds, so none of them has an opinion about a constant
// the emitting code still names and the enumerator has dropped: the value goes
// on being published, documented nowhere and never zeroed.
//
// So the declarations are read directly, and the awkward part is why this is a
// parser rather than a grep. A constant may be declared without an expression,
// in which case Go repeats the previous one; it may be declared as another
// constant's name; and either shape makes two names one value. A reader that
// skips what it cannot immediately evaluate counts one declaration where there
// are two, which balances every count that compares declarations with an
// enumerator — and an alias is exactly how two operational causes quietly
// become one label value.
//
// No dependency on testing, for the reason [rbacdoc] has none.
package vocab

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
)

// StringConstants is every string constant a package's non-test files declare
// whose name carries the given prefix, by name, with aliases resolved.
//
// An empty prefix returns all of them.
func StringConstants(dir, prefix string) (map[string]string, error) {
	files, err := parsePackage(dir)
	if err != nil {
		return nil, err
	}

	declared := declarations(files)

	// Every declaration's literal value, whatever its name, so an alias can be
	// followed to a constant the prefix does not cover. Then identifiers are
	// followed until nothing changes, which resolves a chain of them.
	resolved := map[string]string{}
	for _, d := range declared {
		if lit, ok := d.expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if value, err := strconv.Unquote(lit.Value); err == nil {
				resolved[d.name] = value
			}
		}
	}
	for range len(declared) {
		progressed := false
		for _, d := range declared {
			if _, done := resolved[d.name]; done {
				continue
			}
			ident, ok := d.expr.(*ast.Ident)
			if !ok {
				continue
			}
			if value, ok := resolved[ident.Name]; ok {
				resolved[d.name], progressed = value, true
			}
		}
		if !progressed {
			break
		}
	}

	out := map[string]string{}
	for _, d := range declared {
		if !strings.HasPrefix(d.name, prefix) {
			continue
		}
		value, ok := resolved[d.name]
		if !ok {
			// Reported rather than skipped: skipping is the hole this package
			// exists to close, and a declaration nobody can evaluate is one
			// nobody can hold to an enumerator either.
			return nil, fmt.Errorf("%s declares %s as an expression this cannot evaluate, "+
				"so it would be invisible to every check comparing the declarations with "+
				"an enumerator", dir, d.name)
		}
		out[d.name] = value
	}
	return out, nil
}

// declaration is one name and the expression it was declared with.
type declaration struct {
	expr ast.Expr
	name string
}

// declarations is every name a set of files declares that could hold a string.
func declarations(files []*ast.File) []declaration {
	var out []declaration
	for _, file := range files {
		var previous []ast.Expr
		var previousType ast.Expr
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}

			// A const spec with no expression repeats the previous one — and
			// its type — so a name declared alone is an alias carrying that
			// value, or the next member of an iota run.
			values, typ := spec.Values, spec.Type
			if len(values) == 0 {
				values, typ = previous, previousType
			} else {
				previous, previousType = values, spec.Type
			}
			if len(spec.Names) != len(values) || !mayBeString(typ) {
				return true
			}

			for i, name := range spec.Names {
				// An iota run repeats `iota` into every member, and none of
				// them is a string. Dropped here rather than reported, since
				// the alternative is to call every enum in the package an
				// expression this cannot evaluate.
				if ident, ok := values[i].(*ast.Ident); ok && ident.Name == "iota" {
					continue
				}
				out = append(out, declaration{expr: values[i], name: name.Name})
			}
			return true
		})
	}
	return out
}

// mayBeString reports whether a const spec's type leaves it able to hold one.
//
// An untyped spec can, and `string` can; a named type of any other kind
// cannot.
func mayBeString(typ ast.Expr) bool {
	if typ == nil {
		return true
	}
	ident, ok := typ.(*ast.Ident)
	return ok && ident.Name == "string"
}

// parsePackage parses every non-test file in a directory.
func parsePackage(dir string) ([]*ast.File, error) {
	sources, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, fmt.Errorf("globbing %s: %w", dir, err)
	}

	fset := token.NewFileSet()
	var files []*ast.File
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, source, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", source, err)
		}
		files = append(files, file)
	}

	// A glob no file answers returns an empty list and a nil error, so a
	// package that moves reads as one declaring nothing.
	if len(files) == 0 {
		return nil, fmt.Errorf("no non-test file matched %s/*.go — the package moved, and "+
			"every check reading it is now vacuous", dir)
	}
	return files, nil
}
