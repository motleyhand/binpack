package cli

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/motleyhand/binpack/api/v1alpha1"
	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/mother"
)

// The reference, and the heading of the one paragraph that describes how
// binpack finds a node's pool. Read here for the reason the other doc tests in
// this package are here: a guard over documentation needs a package that
// compiles without a cluster, and the engine may not touch the filesystem.
const (
	configurationReference = "../../docs/reference/configuration.md"
	discoveryHeading       = "### `discovery.nodeGroupIDLabel`"
)

// selfLabelRemedy is the command binpack prints when it can find no pool, and
// the command the reference gives for the same problem — the same characters
// up to the group name, which only the cluster knows.
//
// Shared as one literal checked against both, because a preflight that refuses
// to run and a reference that explains why are useless separately. The
// promise that this check exists at all was in the reference for two releases
// before anything implemented it; the guard below is what stops that
// happening in the other direction.
const selfLabelRemedy = "kubectl label nodes <node>... binpack.motleyhand.com/node-group="

// providerKeyClaims are the phrasings that sent an operator looking for
// something that is not there.
//
// The first points at a provider-supplied label whose values match the
// autoscaler's group names; outside DigitalOcean binpack knows of none, and a
// reader who goes looking finds keys that look right and are not. The other
// two are ADR-0004's second resolution mode, which was published, reasoned
// about in its Consequences, repeated in the architecture document, and never
// built — `v1alpha1.Load` uses UnmarshalStrict, so a document stating a pool
// minimum is rejected outright.
var providerKeyClaims = []string{
	"Other providers use a different key",
	"fall back to configured pool minimums",
	"operator-stated minimums",
}

// withdrawnSentences are the three passages as they stood, kept verbatim so
// that the list above is checked against something rather than against
// nothing.
//
// A removal list cannot fail by itself: a phrase nobody spelled right catches
// nothing, and a phrase deleted from the list stops being checked with no test
// going red. Requiring the two sides to cover each other closes both — every
// claim has to match a sentence that was really there, and every sentence has
// to be matched by some claim.
//
// ADR-0004 still contains the third, under a note marking it superseded. That
// is deliberate and is why the ADR is not one of the sources checked below: a
// withdrawn decision keeps its text, because the record of what was intended
// is worth more than the tidiness of deleting it.
var withdrawnSentences = []string{
	"Defaults to DigitalOcean's. Other providers use a different key; if the values do not " +
		"match the node group names in the status ConfigMap, preflight fails loudly rather " +
		"than guessing.",
	"Other providers need investigating one at a time. Until then, those providers fall back " +
		"to configured pool minimums.",
	"2. **Configured.** Otherwise, fall back to pool membership by label and operator-stated " +
		"minimums.",
}

func TestEveryClaimThisGuardLooksForIsOneThatWasReallyMade(t *testing.T) {
	for _, claim := range providerKeyClaims {
		if !slices.ContainsFunc(withdrawnSentences, func(s string) bool {
			return strings.Contains(s, claim)
		}) {
			t.Errorf("no withdrawn sentence contains %q, so looking for it catches nothing",
				claim)
		}
	}
	for _, sentence := range withdrawnSentences {
		if !slices.ContainsFunc(providerKeyClaims, func(c string) bool {
			return strings.Contains(sentence, c)
		}) {
			t.Errorf("no claim in providerKeyClaims matches %q, so that sentence could be "+
				"written again with nothing going red", sentence)
		}
	}
}

// nodeGroupLabelSources are the four places that tell an operator how binpack
// finds a pool: the reference paragraph, the first-contact page, the design
// document, and the file every installer actually edits.
//
// ADR-0004 is deliberately absent. A superseded decision keeps its text — the
// record is worth more than the tidiness — so it is checked for naming its
// successor instead.
var nodeGroupLabelSources = []string{
	configurationReference,
	"../../README.md",
	"../../docs/design/2026-08-15-architecture.md",
	"../../charts/binpack/values.yaml",
}

// discoverySection returns the `discovery.nodeGroupIDLabel` section of the
// reference and nothing else, so an assertion about the label binpack matches
// on is not answered by a sentence about the one it only prints.
func discoverySection(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(configurationReference)
	if err != nil {
		t.Fatalf("reading %s: %v", configurationReference, err)
	}
	_, after, found := strings.Cut(string(data), discoveryHeading)
	if !found {
		t.Fatalf("%s has no %s section", configurationReference, discoveryHeading)
	}
	section, _, _ := strings.Cut(after, "\n### ")

	// The parse has to be able to fail, or everything built on it passes for
	// the wrong reason. A section that no longer names the key it documents is
	// one this found the wrong boundaries of.
	if !strings.Contains(section, v1alpha1.DefaultNodeGroupIDLabel) {
		t.Fatalf("the %s section does not name the key it defaults to, so this asserts "+
			"nothing:\n%s", discoveryHeading, section)
	}
	return section
}

// TestTheDiscoveryReferenceGivesTheRemedyRatherThanAProviderKey closes the
// misdirection that made the silent failure unrecoverable.
//
// binpack maps a node to a pool by one label's *value*, and that value has to
// be the cloud provider's own identifier for the group — an Auto Scaling group
// name on AWS, a VM Scale Set name on Azure, a full instance-group URL on GCE.
// Whether any label a provider applies happens to carry that is a question
// about the reader's cluster, and telling them one does sends them looking for
// a key rather than applying one.
func TestTheDiscoveryReferenceGivesTheRemedyRatherThanAProviderKey(t *testing.T) {
	section := discoverySection(t)

	for _, claim := range providerKeyClaims {
		if strings.Contains(flattened(section), claim) {
			t.Errorf("the reference still says %q, which points at something binpack has "+
				"no implementation of and no provider is known to supply", claim)
		}
	}
	if !strings.Contains(section, selfLabelRemedy) {
		t.Errorf("the reference does not give the one remedy that works (%q), so an "+
			"operator whose preflight now fails has nowhere to go", selfLabelRemedy)
	}
}

// TestTheRemedyThePreflightPrintsIsTheOneTheReferenceGives keeps the message
// and the document from drifting apart.
//
// This check converts a cluster that ran badly into one that does not run, and
// the only thing that makes that a fair trade is the error ending the problem
// where it is read. An error naming a remedy the reference contradicts is
// worse than either alone.
func TestTheRemedyThePreflightPrintsIsTheOneTheReferenceGives(t *testing.T) {
	nodes := []*corev1.Node{
		mother.Node("ip-10-0-1-11", mother.NodeLabels(map[string]string{
			"eks.amazonaws.com/nodegroup": "workers",
		})),
	}
	s := engine.Snapshot{
		Nodes: nodes,
		Now:   time.Now(),
		Autoscaler: engine.Autoscaler{
			Running:   true,
			LastProbe: time.Now(),
			Groups:    []engine.NodeGroup{{ID: "asg-a", MinSize: 1, MaxSize: 10, Ready: 1}},
		},
	}
	cfg := engine.Config{
		NodeGroupIDLabel: v1alpha1.DefaultNodeGroupIDLabel,
		PoolNameLabel:    v1alpha1.DefaultPoolNameLabel,
	}

	err := engine.CheckPools(s, cfg)
	if err == nil {
		t.Fatal("no node carries the configured label and preflight passed, so there is no " +
			"message for the reference to agree with")
	}
	if !strings.Contains(err.Error(), selfLabelRemedy) {
		t.Errorf("preflight does not print the remedy the reference gives (%q):\n%v",
			selfLabelRemedy, err)
	}

	data, readErr := os.ReadFile(configurationReference)
	if readErr != nil {
		t.Fatalf("reading %s: %v", configurationReference, readErr)
	}
	if !strings.Contains(string(data), selfLabelRemedy) {
		t.Errorf("%s does not carry the remedy binpack prints (%q)",
			configurationReference, selfLabelRemedy)
	}
}

// TestNoLiveRecordStillPromisesAFallbackToConfiguredMinimums checks all four
// surfaces at once because the failure mode is fixing three.
//
// The claim reads naturally in each register — a reference paragraph, a
// selling sentence, a design decision, a YAML comment — and the one nobody
// greps for is the chart's, which is also the one every installer opens.
func TestNoLiveRecordStillPromisesAFallbackToConfiguredMinimums(t *testing.T) {
	for _, source := range nodeGroupLabelSources {
		t.Run(source, func(t *testing.T) {
			data, err := os.ReadFile(source)
			if err != nil {
				t.Fatalf("reading %s: %v", source, err)
			}
			text := flattened(string(data))

			// Guards the guard. A document that stopped naming the setting
			// would pass everything below by saying nothing — and saying
			// nothing is the defect: the one thing that makes binpack work on
			// a cluster the default does not fit was documented in exactly
			// one paragraph, which pointed the wrong way.
			if !strings.Contains(text, "nodeGroupIDLabel") {
				t.Fatalf("%s does not name discovery.nodeGroupIDLabel, so this asserts "+
					"nothing — and an operator reading only this file cannot learn that "+
					"binpack needs a label whose value is the autoscaler's own group name",
					source)
			}
			for _, claim := range providerKeyClaims {
				if strings.Contains(text, claim) {
					t.Errorf("%s still promises %q", source, claim)
				}
			}
		})
	}
}
