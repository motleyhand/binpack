package rbacdoc

import (
	"slices"
	"testing"
)

// TestDocumentsSplitsWhereYAMLDoes holds the separator against what a YAML
// parser accepts, because a line this treats as a separator and YAML does not
// is a stream that will not apply.
//
// `---#comment` is the case: with no space, YAML reads `#` as ordinary content
// rather than a comment indicator and refuses the document. Splitting there
// removed the offending line and handed the decoder two fragments that each
// parse, so a typo of that shape passed every audit while the copyable YAML
// was rejected.
func TestDocumentsSplitsWhereYAMLDoes(t *testing.T) {
	for _, c := range []struct {
		name   string
		stream string
		want   int
	}{
		{"a bare separator", "a: 1\n---\nb: 2\n", 2},
		{"a trailing space", "a: 1\n--- \nb: 2\n", 2},
		{"a spaced comment", "a: 1\n--- # second\nb: 2\n", 2},
		{"a tabbed comment", "a: 1\n---\t# second\nb: 2\n", 2},
		// Not separators.
		{"no space before the comment", "a: 1\n---#second\nb: 2\n", 1},
		{"a document start with content", "a: 1\n--- b: 2\n", 1},
		{"a longer rule", "a: 1\n----\nb: 2\n", 1},
		{"indented", "a: 1\n  ---\nb: 2\n", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := len(Documents(c.stream)); got != c.want {
				t.Errorf("%q splits into %d documents, want %d", c.stream, got, c.want)
			}
		})
	}
}

// TestFencedYAMLRefusesEverySpellingItDoesNotRead is what makes reading only
// one spelling safe.
//
// A block written another way is not a block whose grants go uncompared; it is
// an error naming the line. That is the whole trade: the reader stays a string
// comparison, and the risk it used to carry is paid for here.
func TestFencedYAMLRefusesEverySpellingItDoesNotRead(t *testing.T) {
	read := func(t *testing.T, doc string) []string {
		t.Helper()
		out, err := fencedYAML(doc)
		if err != nil {
			t.Fatalf("reading: %v", err)
		}
		return out
	}

	if got := read(t, "text\n```yaml\na: 1\n```\nmore\n"); !slices.Equal(got, []string{"a: 1"}) {
		t.Errorf("the canonical spelling read as %q", got)
	}
	// A fence that is not YAML at all is not this reader's business.
	if got := read(t, "```bash\nls\n```\n"); len(got) != 0 {
		t.Errorf("a shell block was read as YAML: %q", got)
	}

	for _, c := range []struct{ name, doc string }{
		{"a tilde fence", "~~~yaml\na: 1\n~~~\n"},
		{"the yml spelling", "```yml\na: 1\n```\n"},
		{"a capitalised language", "```YAML\na: 1\n```\n"},
		{"a space before the language", "``` yaml\na: 1\n```\n"},
		{"info-string metadata", "```yaml title=\"x\"\na: 1\n```\n"},
		{"an indented fence", "  ```yaml\n  a: 1\n  ```\n"},
		{"a longer fence", "````yaml\na: 1\n````\n"},
		{"a separator inside", "```yaml\na: 1\n---\nb: 2\n```\n"},
		{"never closed", "```yaml\na: 1\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := fencedYAML(c.doc); err == nil {
				t.Errorf("%q was accepted or ignored; a block this reader does not "+
					"compare has to be an error, or its permissions go unchecked", c.doc)
			}
		})
	}
}
