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
// So the declarations are read directly — by the Go type checker, which is why
// this file is as short as it is. The version this replaces parsed the syntax
// and evaluated the expressions itself, because a constant may be written as a
// concatenation, a conversion to a named string type, an alias, arithmetic
// over named runes, an iota sequence, or a spec with no expression at all that
// repeats the one above it. Each of those is an ordinary way to write a
// vocabulary member; each was found missing in turn, over six review rounds;
// and the evaluator grew to six hundred lines chasing a definition that
// already exists. go/constant is what those expressions mean, and go/types is
// what applies it.
//
// The rule that produced the first version — never skip a constant you cannot
// evaluate, because a skipped one balances both sides of every count that
// compares declarations with an enumerator — is now held by construction. A
// constant either has a value here or its package does not compile, and a
// package that does not compile has already failed the test that called this.
//
// No dependency on testing, for the reason [rbacdoc] has none.
package vocab

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
)

// StringConstants is every string constant a package's non-test files declare
// whose name carries the given prefix, by name.
//
// An empty prefix returns all of them.
//
// Package-level declarations only, which is what the package scope holds. A
// constant declared inside a function is not part of a published vocabulary,
// and counting one would fail the guard that compares declarations with an
// enumerator — reachable through Defs, which is why the scope is read instead.
//
// Typed constants are included, because that is how a vocabulary is usually
// written: `type SkipCode string` and its members are string constants to Go
// and are treated as such here. What is excluded is anything whose value is not
// a string — an integer, a bool, a rune — whatever its name looks like.
func StringConstants(dir, prefix string) (map[string]string, error) {
	fset := token.NewFileSet()

	// Globbed and parsed one file at a time rather than with parser.ParseDir,
	// which is deprecated for a reason that would matter here: it ignores
	// build tags, so a file the compiler excludes would still be read. No file
	// in this module carries one, and this is the shape that does not have to
	// be revisited if one ever does.
	sources, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, fmt.Errorf("globbing %s: %w", dir, err)
	}

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
	// package that moves would otherwise read as one declaring nothing — and
	// a vocabulary that declares nothing agrees with every enumerator.
	if len(files) == 0 {
		return nil, fmt.Errorf("no non-test file matched %s/*.go — the package moved, and "+
			"an empty vocabulary agrees with any enumerator", dir)
	}

	checked, err := check(fset, files[0].Name.Name, files)
	if err != nil {
		return nil, fmt.Errorf("type-checking %s: %w", dir, err)
	}

	scope := checked.Scope()
	if err := everyConstantResolved(dir, files, scope); err != nil {
		return nil, err
	}

	out := map[string]string{}
	for _, declared := range scope.Names() {
		if !strings.HasPrefix(declared, prefix) {
			continue
		}
		value, ok := scope.Lookup(declared).(*types.Const)
		if !ok || value.Val().Kind() != constant.String {
			continue
		}
		out[declared] = constant.StringVal(value.Val())
	}
	return out, nil
}

// check evaluates one package's constants.
//
// Imports are stubbed rather than resolved, and that is the whole reason this
// is fast enough to run in a unit test. Compiling this module's dependencies
// from source to type-check internal/controller took ninety seconds — it pulls
// in most of k8s.io — and reading export data instead needs a build nobody has
// necessarily done. Neither is worth paying for what is being asked here,
// which is the value of a string constant.
//
// The standard library is resolved rather than stubbed, because it is already
// compiled beside the toolchain and costs a fifth of a second — and because
// constants are declared in terms of it: `15 * time.Second` has no value
// without `time`, which the completeness guard below reported the moment
// everything was stubbed.
//
// A stubbed import makes every *other* declaration in the file fail to check,
// so errors are collected and discarded rather than returned. What that would
// hide is a constant that failed for a real reason, and
// [everyConstantResolved] is what refuses to let it: the syntax says how many
// constants a package declares, and every one of them has to come back with a
// value.
func check(fset *token.FileSet, name string, files []*ast.File) (*types.Package, error) {
	config := types.Config{
		Importer:         stubbed{real: importer.Default()},
		IgnoreFuncBodies: true,
		FakeImportC:      true,
		Error:            func(error) {},
	}

	// The error is discarded for the reason above; a package that produced
	// nothing at all still fails, below.
	//
	// The files' own FileSet, not a fresh one: the checker resolves positions
	// through it, and handed an empty one it dereferences a file it cannot
	// find.
	checked, _ := config.Check(name, fset, files, nil)
	if checked == nil {
		return nil, fmt.Errorf("%s produced no package at all", name)
	}
	return checked, nil
}

// stubbed resolves what it can and invents the rest.
type stubbed struct{ real types.Importer }

func (s stubbed) Import(path string) (*types.Package, error) {
	if pkg, err := s.real.Import(path); err == nil {
		return pkg, nil
	}
	return types.NewPackage(path, filepath.Base(path)), nil
}

// everyConstantResolved refuses a package whose constants did not all get a
// value.
//
// The rule the first version of this package was built around, and the only
// thing standing between stubbed imports and a silent answer: a constant this
// cannot read is a constant missing from both sides of every count that
// compares declarations with an enumerator, so the guard passes while the
// value is published and documented nowhere.
//
// Counted from the syntax, because that is the one source that cannot have
// been affected by whatever the type checker made of the file.
func everyConstantResolved(dir string, files []*ast.File, scope *types.Scope) error {
	for _, file := range files {
		for _, decl := range file.Decls {
			block, ok := decl.(*ast.GenDecl)
			if !ok || block.Tok != token.CONST {
				continue
			}
			for _, spec := range block.Specs {
				spec, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range spec.Names {
					if name.Name == "_" {
						continue
					}
					declared, _ := scope.Lookup(name.Name).(*types.Const)
					if declared != nil && declared.Val().Kind() != constant.Unknown {
						continue
					}
					return fmt.Errorf("%s declares the constant %s and its value could "+
						"not be determined; every check that compares declarations "+
						"with an enumerator would be missing it from both sides, and "+
						"pass", dir, name.Name)
				}
			}
		}
	}
	return nil
}
