package cli

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/motleyhand/binpack/api/v1alpha1"
	"github.com/motleyhand/binpack/internal/controller"
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

// The pod must outlive the shutdown it is being asked to perform.
//
// controller-runtime cancels the runnables, waits for them to return — up to
// the graceful window binpack sets — and only then releases the lease. A pod
// SIGKILLed before that window is out is killed at the moment the handover
// would have happened, so the next leader waits out the whole lease instead,
// which is the cost releasing it exists to avoid. Kubernetes' own default is
// 30s, so this is not something a chart can leave unsaid and hope.
func TestTheChartGivesTheShutdownTimeToHappen(t *testing.T) {
	deployment, err := os.ReadFile("../../charts/binpack/templates/deployment.yaml")
	if err != nil {
		t.Fatalf("reading the chart's deployment: %v", err)
	}

	m := regexp.MustCompile(`terminationGracePeriodSeconds:\s*(\d+)`).
		FindStringSubmatch(string(deployment))
	if m == nil {
		t.Fatal("the chart sets no terminationGracePeriodSeconds, so the pod is killed on " +
			"Kubernetes' default and the shutdown has whatever is left of it")
	}
	seconds, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("terminationGracePeriodSeconds is not a number: %v", err)
	}
	grace := time.Duration(seconds) * time.Second

	if grace <= controller.DefaultGracefulShutdown {
		t.Errorf("the pod is killed after %s, inside binpack's own %s shutdown window",
			grace, controller.DefaultGracefulShutdown)
	}
	// And not so long that waiting for the old pod costs more than the lease it
	// is trying to hand over.
	if grace >= controller.DefaultLeaseDuration {
		t.Errorf("a shutdown may take %s, which is no faster than waiting out the %s lease",
			grace, controller.DefaultLeaseDuration)
	}
}
