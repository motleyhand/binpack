package cli

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/motleyhand/binpack/api/v1alpha1"
)

// The reference lives beside the diagnostics one and for the same reason: the
// CLI is what points people at these documents, and it is the package allowed
// to read the filesystem.
const configurationReference = "../../docs/reference/configuration.md"

var yamlBlockRE = regexp.MustCompile("(?s)```yaml\n(.*?)```")

// TestEveryYAMLExampleInTheConfigurationReferenceLoads runs the document's own
// examples through the loader.
//
// An operator copies these. A wrong one does not degrade to a default — the
// loader is strict, so a configuration binpack cannot parse is a binpack that
// does not start, and the reader has no reason to suspect the document rather
// than themselves. Prose about the format can only be checked by reading;
// examples can be executed, so they are.
//
// Uniform leading indentation is stripped, because some examples are nested
// inside a list item in the Markdown and the indentation is the document's
// layout rather than the configuration's.
func TestEveryYAMLExampleInTheConfigurationReferenceLoads(t *testing.T) {
	data, err := os.ReadFile(configurationReference)
	if err != nil {
		t.Fatalf("reading the configuration reference: %v", err)
	}

	matches := yamlBlockRE.FindAllStringSubmatch(string(data), -1)
	if len(matches) == 0 {
		t.Fatal("no yaml examples found: the extraction is wrong, not the document")
	}

	for _, m := range matches {
		block := dedent(m[1])
		first, _, _ := strings.Cut(strings.TrimSpace(block), "\n")

		t.Run(first, func(t *testing.T) {
			if _, err := v1alpha1.Load([]byte(block)); err != nil {
				t.Errorf("this example does not load:\n%s\n%v", block, err)
			}
		})
	}
}

// dedent removes the smallest indentation common to every non-blank line.
func dedent(s string) string {
	lines := strings.Split(s, "\n")

	width := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if n := len(l) - len(strings.TrimLeft(l, " ")); width < 0 || n < width {
			width = n
		}
	}
	if width <= 0 {
		return s
	}

	for i, l := range lines {
		if len(l) >= width {
			lines[i] = l[width:]
		}
	}
	return strings.Join(lines, "\n")
}
