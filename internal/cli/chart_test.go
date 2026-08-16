package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/motleyhand/binpack/api/v1alpha1"
)

// The chart's default configuration must be one binpack accepts.
//
// A chart that renders a document the binary rejects is a crash loop nobody
// discovers until they install it, and `helm lint` cannot see it: the config
// is an opaque string as far as Helm is concerned. This checks the values the
// chart ships, which is what almost every install will actually run.
func TestTheChartsDefaultConfigurationIsValid(t *testing.T) {
	values, err := os.ReadFile("../../charts/binpack/values.yaml")
	if err != nil {
		t.Fatalf("reading the chart's values: %v", err)
	}

	// The `config:` block, rendered into the ConfigMap verbatim.
	block, ok := section(string(values), "config:")
	if !ok {
		t.Fatal("the chart has no config: block to render")
	}

	cfg, err := v1alpha1.Load([]byte(
		"apiVersion: binpack.motleyhand.com/v1alpha1\nkind: BinpackConfig\n" + block))
	if err != nil {
		t.Fatalf("the chart's default configuration is one binpack rejects: %v", err)
	}

	// Acting is opt-in. A chart that shipped dryRun: false would drain nodes
	// on install, which is not a thing anyone should be able to do by accident.
	if s := cfg.Settings(); s.DryRun != true {
		t.Error("the chart does not default to dry run")
	}
}

// section returns the indented body following a top-level key, de-indented.
func section(doc, key string) (string, bool) {
	lines := strings.Split(doc, "\n")
	var out []string
	for i, line := range lines {
		if line != key {
			continue
		}
		for _, body := range lines[i+1:] {
			if body == "" || strings.HasPrefix(body, "  ") {
				out = append(out, strings.TrimPrefix(body, "  "))
				continue
			}
			if strings.HasPrefix(body, "#") {
				continue
			}
			break
		}
		return strings.Join(out, "\n"), true
	}
	return "", false
}
