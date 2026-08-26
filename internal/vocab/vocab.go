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

	runeConstants = runes(files)
	stringTypeNames = stringTypes(files)

	declared := declarations(files, stringTypeNames)

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
			if value, ok := evaluate(d.expr, resolved); ok {
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

// evaluate reads a constant string expression, given what is resolved so far.
//
// Literals, names, parentheses and concatenation — the whole of what a string
// constant can be built from without a function call, which a constant cannot
// contain. Concatenation is here because leaving it out failed the guard on
// `const SkipFoo = "foo" + "-bar"`, which is a legal way to write a value and
// not one this package has any business refusing.
//
// go/types would answer this exactly and would mean type-checking the package
// and its imports to read four constants; the subset a vocabulary is written
// in is small enough to walk, and anything outside it is still reported rather
// than skipped.
func evaluate(expr ast.Expr, resolved map[string]string) (string, bool) {
	switch expr := expr.(type) {
	case *ast.BasicLit:
		if expr.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(expr.Value)
		return value, err == nil
	case *ast.Ident:
		value, ok := resolved[expr.Name]
		return value, ok
	case *ast.ParenExpr:
		return evaluate(expr.X, resolved)
	case *ast.BinaryExpr:
		if expr.Op != token.ADD {
			return "", false
		}
		left, ok := evaluate(expr.X, resolved)
		if !ok {
			return "", false
		}
		right, ok := evaluate(expr.Y, resolved)
		return left + right, ok
	case *ast.CallExpr:
		// A constant conversion, which is the one call a constant may
		// contain: `string('x')` and `string(SomeConst)` are both legal ways
		// to write a vocabulary value, and both reached the default branch and
		// were reported as unevaluable.
		return convertedString(expr, resolved)
	default:
		return "", false
	}
}

// convertedString reads `string(x)` where x is a constant this can evaluate.
//
// A rune literal converts to the character it names; anything already a string
// converts to itself. Any other conversion is not a string and is left to the
// caller to report.
func convertedString(call *ast.CallExpr, resolved map[string]string) (string, bool) {
	fn, ok := call.Fun.(*ast.Ident)
	if !ok || !convertsToString(fn.Name) || len(call.Args) != 1 {
		return "", false
	}

	if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.CHAR {
		runes, err := strconv.Unquote(lit.Value)
		return runes, err == nil
	}
	// A named rune is as legal an operand as a literal one, and those names
	// are not in `resolved` — that map holds strings, and a `const letter rune
	// = 'x'` is filtered out before it reaches one. So the runes are collected
	// separately and consulted here.
	if ident, ok := call.Args[0].(*ast.Ident); ok {
		if value, ok := runeConstants[ident.Name]; ok {
			return value, true
		}
	}
	return evaluate(call.Args[0], resolved)
}

// runeConstants are the package's rune constants, by name, for the conversions
// that name one.
var runeConstants map[string]string

// stringTypeNames are the package's own names for a string, for the
// conversions that name one.
//
// A vocabulary is often declared through its own type, and
// `DecisionCode("Drain")` is as ordinary a way to write a member as
// `DecisionCode = "Drain"`. Recognising only the predeclared `string` read
// those as conversions to something else and dropped them — silently, which is
// the failure this package exists to prevent: a constant omitted from the
// declarations shrinks both sides of the count that holds the enumerator
// honest, so the guard passes while the value is published and documented
// nowhere.
var stringTypeNames map[string]bool

// runes is every constant declared as a character literal, whatever its type.
//
// Collected apart from the string declarations because those are filtered by
// type — a `const letter rune = 'x'` is deliberately not a string — and a
// `string(letter)` conversion still needs it.
func runes(files []*ast.File) map[string]string {
	out := map[string]string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			block, ok := decl.(*ast.GenDecl)
			if !ok || block.Tok != token.CONST {
				continue
			}
			for _, spec := range block.Specs {
				spec, ok := spec.(*ast.ValueSpec)
				if !ok || len(spec.Names) != len(spec.Values) {
					continue
				}
				for i, name := range spec.Names {
					lit, ok := spec.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.CHAR {
						continue
					}
					if value, err := strconv.Unquote(lit.Value); err == nil {
						out[name.Name] = value
					}
				}
			}
		}
	}
	return out
}

// declaration is one name and the expression it was declared with.
type declaration struct {
	expr ast.Expr
	name string
}

// stringTypes are the package's own names for a string, alias or defined.
//
// `type EventReason = string` and `type EventReason string` both give
// constants a type whose values are strings, and a spec typed with either was
// being skipped as though it could not hold one. Skipping is the hole: the
// declaration vanishes from the count that compares declarations with an
// enumerator, and both sides shrink together when the value is dropped from
// the enumerator too.
//
// Resolved transitively, since a name may be declared in terms of another.
func stringTypes(files []*ast.File) map[string]bool {
	// Package-level declarations only, for the reason [declarations] reads
	// only package-level const blocks: ast.Inspect also visits a type declared
	// inside a function, and a local `type EventReason int` would overwrite
	// the package's `type EventReason = string` under the same name — making
	// every constant typed with it look non-string, and dropping them all from
	// the count that holds the enumerator honest. The fix for constants was
	// made a round before this one and not carried across.
	direct := map[string]string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			block, ok := decl.(*ast.GenDecl)
			if !ok || block.Tok != token.TYPE {
				continue
			}
			for _, spec := range block.Specs {
				spec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if ident, ok := spec.Type.(*ast.Ident); ok {
					direct[spec.Name.Name] = ident.Name
				}
			}
		}
	}

	out := map[string]bool{"string": true}
	for range len(direct) + 1 {
		progressed := false
		for name, underlying := range direct {
			if !out[name] && out[underlying] {
				out[name], progressed = true, true
			}
		}
		if !progressed {
			break
		}
	}
	return out
}

// declarations is every name a set of files declares that could hold a string.
func declarations(files []*ast.File, stringy map[string]bool) []declaration {
	var out []declaration
	for _, file := range files {
		// Package-level `const` blocks and nothing else. A ValueSpec is also a
		// `var`, and a function-local one at that, so walking every node made
		// an implementation detail part of the published vocabulary: a
		// `var CodeFormatter = "compact"` in internal/engine would be returned
		// for the prefix `Code` and fail the declaration guard until it was
		// renamed or exposed as a decision code. A guard that fires on correct
		// code is one somebody switches off.
		for _, decl := range file.Decls {
			block, ok := decl.(*ast.GenDecl)
			if !ok || block.Tok != token.CONST {
				continue
			}

			// Reset per block, which is Go's own rule: a spec repeats the
			// previous one within a const block and never across two.
			var previous []ast.Expr
			var previousType ast.Expr
			for _, spec := range block.Specs {
				spec, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}

				// A const spec with no expression repeats the previous one —
				// and its type — so a name declared alone is an alias carrying
				// that value, or the next member of an iota run.
				values, typ := spec.Values, spec.Type
				if len(values) == 0 {
					values, typ = previous, previousType
				} else {
					previous, previousType = values, spec.Type
				}
				if len(spec.Names) != len(values) || !mayBeString(typ, stringy) {
					continue
				}

				for i, name := range spec.Names {
					// A constant that is plainly not a string is not a member
					// of any vocabulary, and reporting it as unevaluable makes
					// the guard fail on correct code: `const CodeRadix = 10`
					// matches the prefix `Code` and is an integer. An iota run
					// is the same case — it repeats `iota` into every member.
					if plainlyNotAString(values[i]) {
						continue
					}
					out = append(out, declaration{expr: values[i], name: name.Name})
				}
			}
		}
	}
	return out
}

// convertsToString reports whether a conversion to this type name yields a
// string: the predeclared one, or any of the package's own names for it.
func convertsToString(name string) bool {
	return name == "string" || stringTypeNames[name]
}

// plainlyNotAString reports whether an expression obviously holds something
// else.
//
// Only the cases that can be read off the syntax. Anything subtler is left to
// the resolver, which reports what it cannot evaluate rather than skipping it
// — the two halves are deliberate: skipping what might be a string is the hole
// this package exists to close, and reporting what is plainly an integer is a
// guard nobody keeps.
func plainlyNotAString(expr ast.Expr) bool {
	switch expr := expr.(type) {
	case *ast.BasicLit:
		return expr.Kind != token.STRING
	case *ast.Ident:
		return expr.Name == "iota" || expr.Name == "true" || expr.Name == "false"
	case *ast.ParenExpr:
		return plainlyNotAString(expr.X)
	case *ast.UnaryExpr:
		// No unary operator applies to a string.
		return true
	case *ast.BinaryExpr:
		// Classified by the operator first, because the operator decides the
		// *result*. Only `+` yields a string; every comparison yields a bool
		// and every arithmetic or bitwise operator yields a number, whatever
		// the operands are — so `"a" == "a"` is plainly not a string even
		// though both sides are, and reading the operands alone reported it as
		// one this could not evaluate. That failed the vocabulary guard on
		// valid package code whose name happened to share a prefix.
		if expr.Op != token.ADD {
			return true
		}
		// And for `+`, Go permits no operator between a string and anything
		// else, so one plainly non-string operand settles it. `1 + 2` is
		// caught here; `1 << 4` is caught above, having reached the default
		// branch before either check existed and failed a correct
		// `const CodeMask`.
		return plainlyNotAString(expr.X) || plainlyNotAString(expr.Y)
	case *ast.CallExpr:
		// A constant may contain a conversion and nothing else, so a call to
		// anything but a string type converts to a type that is not one — and
		// the package's own names for a string count, which is how a
		// vocabulary is usually spelled. See [stringTypeNames].
		fn, ok := expr.Fun.(*ast.Ident)
		return !ok || !convertsToString(fn.Name)
	default:
		return false
	}
}

// mayBeString reports whether a const spec's type leaves it able to hold one.
//
// An untyped spec can, `string` can, and so can any of the package's own names
// for a string. A named type of any other kind cannot.
func mayBeString(typ ast.Expr, stringy map[string]bool) bool {
	if typ == nil {
		return true
	}
	ident, ok := typ.(*ast.Ident)
	return ok && stringy[ident.Name]
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
