package cli

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/motleyhand/binpack/internal/rbacdoc"
)

// The surfaces a stranger meets: the page GitHub renders, the two references
// and the how-to they reach from it, and the notes Helm prints at the end of
// every install.
//
// They live here rather than beside the chart or the docs because this is the
// package that already reads both — see chart_test.go and
// diagnostics_doc_test.go — and because a doc test needs a package that
// compiles without a cluster.
var userFacingSurfaces = []string{
	"../../README.md",
	"../../docs/how-to/install-binpack.md",
	"../../docs/reference/configuration.md",
	"../../docs/reference/rbac.md",
	"../../charts/binpack/templates/NOTES.txt",
}

// Sentences that were true before the executor shipped and have been false in
// every tagged release since.
//
// A phrase list rather than a pattern for "status banner", deliberately. The
// broad version of this test matches docs/reference/versioning.md, whose 0.x
// policy — the version stays at 0.x until binpack has acted on somebody
// else's cluster — is a policy that still holds, and the quick-wins page,
// which tells a reader they might not need binpack at all. Both should keep
// saying what they say. What must not come back is a claim that this build
// cannot do what it does.
var deniesActing = []string{
	"nothing is installable",
	"not yet used",
	"nothing in this build cordons",
	"until a build that can act",
	"this build never writes those",
	"until the controller lands",
}

func TestNoDocClaimsBinpackCannotAct(t *testing.T) {
	for _, path := range userFacingSurfaces {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		doc := flatten(string(data))

		for _, claim := range deniesActing {
			if strings.Contains(doc, claim) {
				t.Errorf("%s says %q, and binpack cordons nodes and evicts pods "+
					"whenever dryRun is false", path, claim)
			}
		}
	}
}

// TestTheRBACReferenceMatchesWhatTheExecutorDoes checks the third side of an
// agreement the chart and the code already keep between them.
//
// An operator managing RBAC outside the chart — rbac.create: false, which the
// chart supports on purpose — writes their role from the reference, and the
// chart's render-time refusal cannot see what they granted. So a verb the
// executor needs that the page files under "not needed" is a role missing it,
// and a drain that 403s on its first node patch: binpack holds its lease,
// serves its metrics, publishes decisions, and never drains anything.
func TestTheRBACReferenceMatchesWhatTheExecutorDoes(t *testing.T) {
	// The rules gated on rbac.allowDraining: what internal/executor needs, as
	// the chart grants it. Taken from the chart rather than listed here, so
	// the day a third verb is added the reference has to say so too — and as
	// the difference between an install that opted in and one that did not,
	// which is the operator's own reading of what opting in buys.
	on, err := rbacdoc.Roles(rbacdoc.MustRender(t.Fatal, rbacdoc.Options{
		Set: map[string]string{"rbac.allowDraining": "true"},
	}))
	if err != nil {
		t.Fatalf("reading the chart's rules: %v", err)
	}
	off, err := rbacdoc.Roles(rbacdoc.MustRender(t.Fatal, rbacdoc.Options{
		Set: map[string]string{"rbac.allowDraining": "false"},
	}))
	if err != nil {
		t.Fatalf("reading the chart's ungated rules: %v", err)
	}

	// Narrowed to the ClusterRole, which is where the act rules live and so
	// the scope the page has to document them at.
	granted := rbacdoc.Difference(
		rbacdoc.OfKind(on, "ClusterRole"), rbacdoc.OfKind(off, "ClusterRole"))
	// A count rather than a non-empty check. The act block has held two pairs
	// since it existed, and a reader that found one would compare that one and
	// pass — which is the whole failure this guard is here for.
	//
	// Two ways to get here and the message names both, because they call for
	// opposite responses: the reader has stopped seeing a rule, or a rule has
	// left the guard. The second is the more alarming and the less obvious —
	// internal/controller's TestTheChartGrantsWhatTheCodeCalls is what says
	// which, since it holds the act grants to the writes binpack makes only
	// when acting.
	if len(granted) < 2 {
		t.Fatalf("the chart's act rules parsed to %d group/resource: verb pairs, fewer "+
			"than the two it has always rendered. Either the reader has stopped seeing "+
			"a rule and the comparison below is against the remainder, or a rule has "+
			"moved out of the rbac.allowDraining guard and a default install now holds "+
			"it", len(granted))
	}

	documented := documentedGrants(t)["ClusterRole"]

	for pair := range granted {
		if !documented[pair] {
			t.Errorf("the chart grants %q in its ClusterRole to let binpack act, and the "+
				"RBAC reference does not list it there outside a section marked unused",
				pair)
		}
	}
}

// TestTheRBACReferenceIsACompleteRoleSpecification holds the page to what its
// own banner promises: that it is the page to write a role from.
//
// Codex found the gap this closes. The reference showed the leader-election
// Role as leases alone, while the chart also grants core `events` there —
// client-go's leader-election recorder writes to the *core* group, unlike the
// decision events. The page explained that in prose four paragraphs below the
// YAML block that omitted it, so a role copied from the block logged an
// authorization failure the same page had already predicted.
//
// The pairs carry their API group for that reason. Keyed on the resource
// alone, `events` from `events.k8s.io` and `events` from the core group are
// the same string, and this test would have been blind to the one gap it
// exists to catch.
func TestTheRBACReferenceIsACompleteRoleSpecification(t *testing.T) {
	// Every rule the chart can render, which is an install that opted in to
	// both features: the page documents the whole role, and an operator
	// managing RBAC themselves needs all of it. Both values are named because
	// both default to off for the gated rules, and a default render would hold
	// the page to a subset while claiming to be complete.
	roles, err := rbacdoc.Roles(rbacdoc.MustRender(t.Fatal, rbacdoc.Options{
		Set: map[string]string{
			"rbac.allowDraining":     "true",
			"leaderElection.enabled": "true",
		},
	}))
	if err != nil {
		t.Fatalf("reading the chart's rules: %v", err)
	}
	// A count, in chart_test.go's shape rather than a non-empty check. The
	// comparisons below iterate parsed sets, so a reader that stopped seeing
	// rules would check the ones it still saw and report nothing about the
	// rest — a green run that means less than it did, with nothing to say so.
	//
	// What it does not catch is a rule *deleted* from the chart, which shrinks
	// the set for a reason the count cannot distinguish from the reader's. The
	// equalities below are what catch that.
	if all := rbacdoc.Grants(roles); len(all) < 30 {
		t.Fatalf("the chart parsed to %d group/resource: verb pairs, which is fewer than "+
			"it has ever rendered — the reader has stopped seeing rules, and the "+
			"comparison below is against the remainder", len(all))
	}

	// Both directions, and per kind. chart ⊆ page says a role written from the
	// page is not missing a grant; page ⊆ chart says it does not carry one the
	// chart never renders. Only the pair of them says the page *is* the role,
	// which is what its banner claims — and only the second notices a rule
	// deleted from the chart, which the first reads as one fewer thing to
	// check. Deleting the autoscaler-status ConfigMap rule cost three pairs
	// and left every assertion here green.
	//
	// Per kind because the union cannot express where a namespaced grant
	// applies; see documentedGrants.
	documented := documentedGrants(t)
	for _, identity := range rbacdoc.Identities() {
		granted := rbacdoc.Grants(rbacdoc.OfIdentity(roles, identity))
		if len(granted) == 0 {
			t.Fatalf("the chart renders no %s rules at all", identity)
		}
		for pair := range granted {
			if !documented[identity][pair] {
				t.Errorf("the chart's %s grants %q and the RBAC reference does not list it "+
					"there; a role written from this page would be missing it",
					identity, pair)
			}
		}
		for pair := range documented[identity] {
			if !granted[pair] {
				t.Errorf("the RBAC reference lists %q under %s outside a section marked "+
					"unused and no such rule the chart renders grants it; either the chart "+
					"has stopped granting something binpack needs, or the page is asking "+
					"for a permission nobody has to give", pair, identity)
			}
		}
	}

	// And a property that is not a comparison at all: a rule can be wrong on
	// its own terms, and two documents agreeing about it says nothing.
	for _, grant := range rbacdoc.Unauthorizable(roles) {
		t.Errorf("%s — these verbs carry no object name for a resourceNames restriction "+
			"to match, and no field selector supplies one, so this grant authorises "+
			"nothing at all", grant)
	}
}

// documentedGrants is what docs/reference/rbac.md tells an operator to grant,
// in the role kind it tells them to grant it in.
//
// Kept apart rather than merged, because merged they answer a question no
// operator asks. A cluster-wide grant applies everywhere; a namespaced one
// applies in the namespace its Role is created in, and the page creates Roles
// in two of them. Moving the events.k8s.io rule out of the ClusterRole snippet
// and into the leader-election Role's left both directions of the comparison
// green, and a role written from the page would grant it in binpack's own
// namespace while decision Events about a Node are filed under `default`.
func documentedGrants(t *testing.T) map[string]map[string]bool {
	t.Helper()

	data, err := os.ReadFile(rbacdoc.ReferencePath)
	if err != nil {
		t.Fatalf("reading the RBAC reference: %v", err)
	}
	roles, err := rbacdoc.Documented(grantedSections(string(data)))
	if err != nil {
		t.Fatalf("reading the RBAC reference's rules: %v", err)
	}

	out, placed := map[string]map[string]bool{}, 0
	for _, identity := range rbacdoc.Identities() {
		at := rbacdoc.OfIdentity(roles, identity)
		placed += len(at)
		out[identity] = rbacdoc.Grants(at)
	}

	// A snippet this reader cannot place is one it would otherwise compare at
	// the wrong scope, which is the failure above. The page states the object
	// on every block; a new one that does not — or one naming a Role
	// rbacdoc.NamespacedRoles has never heard of — has to say so before it can
	// be compared.
	if placed != len(roles) {
		t.Fatalf("%d of the reference's %d rule blocks do not name an object this can "+
			"place, so it cannot say which scope they are granted at; the blocks are "+
			"headed `# ClusterRole` or `# Role, named <release>%v, …`",
			len(roles)-placed, len(roles), rbacdoc.NamespacedRoles)
	}
	return out
}

// grantedSections drops the sections of a document whose heading says the
// permissions in them are not granted or not needed, so what remains is what
// the page tells an operator to grant.
func grantedSections(doc string) string {
	var kept []string
	for i, part := range strings.Split(doc, "\n## ") {
		heading, _, _ := strings.Cut(part, "\n")
		if i > 0 && notGranted.MatchString(heading) {
			continue
		}
		kept = append(kept, part)
	}
	return strings.Join(kept, "\n")
}

var notGranted = regexp.MustCompile(`(?i)not yet|unused|not needed|never granted`)

var whitespace = regexp.MustCompile(`\s+`)

// flatten lower-cases a document and collapses every run of whitespace to one
// space.
//
// Both halves are load-bearing rather than tidiness. These pages wrap at 95
// columns, so "and this build\nnever writes those" is one sentence split
// across two lines and a search for the unwrapped form finds nothing —
// a test that passes against the exact page it was written to catch. The
// claim about cordoning likewise appears capitalised in one place on the
// RBAC page and lower-cased in another.
func flatten(doc string) string {
	return whitespace.ReplaceAllString(strings.ToLower(doc), " ")
}
