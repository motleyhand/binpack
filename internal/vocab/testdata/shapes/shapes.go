// Package shapes is every way binpack's own code has spelled a vocabulary
// member, and several ways it has not.
//
// A fixture rather than a table of source strings, because the thing under
// test is what Go makes of a package: it has to compile, and if one of these
// stops compiling it has stopped being a shape anybody could write.
package shapes

// The plain case, and the one every reader got right.
const CodePlain = "plain"

// Concatenation, and an alias of a constant the prefix does not cover.
const separator = "-"
const CodeJoined = "two" + separator + "words"
const CodeAlias = CodePlain

// A spec with no expression repeats the one above it, which makes two names
// one value.
const (
	CodeFirst = "first"
	CodeRepeated
)

// Conversions: from a literal rune, from a named one, from an arithmetic
// expression over one, and through the package's own name for a string.
type Code string

const letter rune = 65

const (
	CodeFromLiteral = string('x')
	CodeFromRune    = string(letter)
	CodeFromArith   = string(letter + 1)
	CodeConverted   = Code("converted")
)

// An iota sequence, which is how a run of related members is written.
const (
	CodeIotaA = string('A' + iota)
	CodeIotaB
	CodeIotaC
)

// And things that carry the prefix and are not vocabulary members. Each of
// these failed a reader at some point by being skipped, or by being reported
// as an expression it could not evaluate.
const (
	CodeRadix    = 10
	CodeMask     = 1 << 4
	CodeCompared = "a" == "a"
	CodeRuneOnly = 'z'
)

// Not package level, so not a member of anything published.
func local() string {
	const CodeLocal = "local"
	return CodeLocal
}

var _ = local
