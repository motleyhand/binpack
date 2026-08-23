package metrics

import (
	"os"
	"strings"
	"testing"

	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/motleyhand/binpack/internal/drain"
	"github.com/motleyhand/binpack/internal/engine"
)

const reference = "../../docs/reference/metrics.md"

func referenceText(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(reference)
	if err != nil {
		t.Fatalf("reading the metrics reference: %v", err)
	}
	return string(data)
}

func TestEveryPublishedMetricIsDocumented(t *testing.T) {
	// These names are public API. A hand-written reference drifts from the
	// code it describes; this is the check that stops it.
	Observe(snapshot(engine.NodeGroup{ID: "da8977ba-244f", MinSize: 1, MaxSize: 10, Ready: 1}),
		engine.Decision{
			Code:        engine.CodeNoneFeasible,
			Assessments: []engine.NodeAssessment{assess(engine.VerdictSkipped, engine.SkipCordoned)},
		}, config(), 0.01)

	doc := referenceText(t)
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}

	for _, f := range families {
		name := f.GetName()
		if !strings.HasPrefix(name, "binpack_") {
			continue
		}
		// Histograms are exposed as _bucket, _sum and _count; the family
		// carries the documented name.
		if !strings.Contains(doc, "`"+name+"`") {
			t.Errorf("%s is published but not documented", name)
		}
	}
}

func TestTheReferenceDocumentsNoMetricThatDoesNotExist(t *testing.T) {
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	published := map[string]bool{}
	for _, f := range families {
		published[f.GetName()] = true
	}

	doc := referenceText(t)
	for field := range strings.SplitSeq(doc, "`") {
		// The bare prefix appears in prose; it is not a metric name.
		if !strings.HasPrefix(field, "binpack_") || field == "binpack_" {
			continue
		}
		// PromQL examples wrap a name in functions and comparisons; take the
		// bare identifier.
		name := strings.FieldsFunc(field, func(r rune) bool {
			return r != '_' && (r < 'a' || r > 'z')
		})[0]
		if !published[name] {
			t.Errorf("the reference documents %q, which binpack does not publish", name)
		}
	}
}

func TestEveryLabelValueBinpackCanProduceIsDocumented(t *testing.T) {
	// The codes are the vocabulary shared between a dashboard and an
	// investigation. One missing from the reference is one nobody can look up
	// when it appears on a graph at three in the morning.
	//
	// Named for binpack rather than the engine, because the engine is not the
	// only package that produces one: the reason values on
	// binpack_drains_abandoned_total are drain constants.
	doc := referenceText(t)

	// The engine's half is hand-written; the drain half is taken from the
	// enumerator, because there is one. TestEveryAbandonReasonHasAZeroSeries
	// already holds AbandonCodes against the *scrape*, so the reason values
	// were published and documented and nothing joined the two — editing a
	// row of the abandonment table left the suite green.
	codes := []string{
		engine.CodeDrain, engine.CodeNoAutoscaler, engine.CodeAutoscalerUnhealthy,
		engine.CodeNoCandidates, engine.CodeNoneFeasible, engine.CodeDraining,
		engine.VerdictSkipped, engine.VerdictInfeasible, engine.VerdictBlocked, engine.VerdictDrainable,
		engine.SkipNotAutoscaled, engine.SkipPoolDisabled, engine.SkipScaleUpInProgress,
		engine.SkipCooldownAfterScaleUp, engine.SkipCooldownAfterDrain, engine.SkipPoolAtMinimum,
		engine.SkipAnnotated, engine.SkipDrainInProgress, engine.SkipBackoff,
		engine.SkipCordoned, engine.SkipProtectedPod, engine.SkipTooManyPods,
	}
	codes = append(codes, drain.AbandonCodes()...)

	for _, code := range codes {
		if !strings.Contains(doc, "`"+code+"`") {
			t.Errorf("label value %q is not documented", code)
		}
	}
}
