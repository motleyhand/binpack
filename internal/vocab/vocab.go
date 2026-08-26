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

	integerConstants = integers(files)
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

	// A character literal is the operand's commonest shape and the only one
	// this handled at first. Every other way of writing the same constant —
	// a named rune, a conversion, parentheses, arithmetic over them — is as
	// legal, converts to the same string, and was reported as unevaluable,
	// which fails the vocabulary guard over code that compiles. So the operand
	// is evaluated as an integer expression, and only what is not one falls
	// through to the string evaluator.
	//
	// Integers are kept apart from `resolved` because that map holds strings:
	// a `const letter rune = 'x'` is filtered out by type long before it could
	// reach one.
	if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.CHAR {
		runes, err := strconv.Unquote(lit.Value)
		return runes, err == nil
	}
	if value, ok := integer(call.Args[0], integerConstants); ok {
		return string(rune(value)), true
	}
	return evaluate(call.Args[0], resolved)
}

// integerConstants are the package's integer-valued constants, by name, for
// the conversions whose operand is one.
var integerConstants map[string]int64

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

// integers is every constant with an integer value, by name.
//
// Resolved to a fixpoint, because a constant may be defined in terms of one
// declared later in the file or in another of the package\'s files, and because
// an alias of an alias is as ordinary as an alias.
func integers(files []*ast.File) map[string]int64 {
	pending := map[string]ast.Expr{}
	for _, file := range files {
		for _, decl := range file.Decls {
			block, ok := decl.(*ast.GenDecl)
			if !ok || block.Tok != token.CONST {
				continue
			}
			// A spec with no expression repeats the previous one, which
			// [declarations] already carries for the string constants and this
			// dropped. `const ( letter rune = 65; repeated )` left `repeated`
			// unknown, so `string(repeated)` was reported as unevaluable —
			// failing the vocabulary guard over a refactor Go compiles.
			var previous []ast.Expr
			for _, spec := range block.Specs {
				spec, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				values := spec.Values
				if len(values) == 0 {
					values = previous
				} else {
					previous = values
				}
				if len(spec.Names) != len(values) {
					continue
				}
				for i, name := range spec.Names {
					pending[name.Name] = values[i]
				}
			}
		}
	}

	out := map[string]int64{}
	for grew := true; grew; {
		grew = false
		for name, expr := range pending {
			value, ok := integer(expr, out)
			if !ok {
				continue
			}
			out[name] = value
			delete(pending, name)
			grew = true
		}
	}
	return out
}

// integerTypes are the predeclared integer types a constant conversion may
// name.
//
// The predeclared ones only. A package's own name for an integer would need
// resolving the way [stringTypes] resolves its own, and no vocabulary in this
// repository is written that way — so it is left to be reported as unevaluable
// rather than guessed at, which is the standing rule here.
var integerTypes = map[string]bool{
	"rune": true, "byte": true, "int": true, "int8": true, "int16": true,
	"int32": true, "int64": true, "uint": true, "uint8": true, "uint16": true,
	"uint32": true, "uint64": true, "uintptr": true,
}

// integer evaluates a constant integer expression against what is known.
//
// The same shape as [evaluate] and for the same reason: the subset a constant
// may contain is small enough to walk, and what falls outside it is reported as
// unknown rather than guessed at.
func integer(expr ast.Expr, known map[string]int64) (int64, bool) {
	switch expr := expr.(type) {
	case *ast.BasicLit:
		switch expr.Kind {
		case token.INT:
			value, err := strconv.ParseInt(expr.Value, 0, 64)
			return value, err == nil
		case token.CHAR:
			text, err := strconv.Unquote(expr.Value)
			if err != nil {
				return 0, false
			}
			runes := []rune(text)
			if len(runes) != 1 {
				return 0, false
			}
			return int64(runes[0]), true
		default:
			return 0, false
		}
	case *ast.Ident:
		value, ok := known[expr.Name]
		return value, ok
	case *ast.ParenExpr:
		return integer(expr.X, known)
	case *ast.CallExpr:
		// A conversion to an integer type, which is the one call a constant
		// may contain and is how a rune is usually spelled in a conversion:
		// `string(rune(65))`. Any other name converts to something this
		// cannot read, and says so.
		fn, ok := expr.Fun.(*ast.Ident)
		if !ok || len(expr.Args) != 1 || !integerTypes[fn.Name] {
			return 0, false
		}
		return integer(expr.Args[0], known)
	case *ast.UnaryExpr:
		value, ok := integer(expr.X, known)
		switch {
		case !ok:
			return 0, false
		case expr.Op == token.SUB:
			return -value, true
		case expr.Op == token.ADD:
			return value, true
		case expr.Op == token.XOR:
			return ^value, true
		default:
			return 0, false
		}
	case *ast.BinaryExpr:
		left, leftOK := integer(expr.X, known)
		right, rightOK := integer(expr.Y, known)
		if !leftOK || !rightOK {
			return 0, false
		}
		switch expr.Op {
		case token.ADD:
			return left + right, true
		case token.SUB:
			return left - right, true
		case token.MUL:
			return left * right, true
		case token.QUO:
			if right == 0 {
				return 0, false
			}
			return left / right, true
		case token.REM:
			if right == 0 {
				return 0, false
			}
			return left % right, true
		case token.SHL:
			if right < 0 || right > 62 {
				return 0, false
			}
			return left << right, true
		case token.SHR:
			if right < 0 || right > 62 {
				return 0, false
			}
			return left >> right, true
		case token.AND:
			return left & right, true
		case token.OR:
			return left | right, true
		case token.XOR:
			return left ^ right, true
		default:
			return 0, false
		}
	default:
		return 0, false
	}
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
