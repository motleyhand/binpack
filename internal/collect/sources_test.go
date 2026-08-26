package collect_test

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/motleyhand/binpack/internal/collect"
	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/rbacdoc"
)

// The chart must grant read on exactly the controller kinds binpack reads
// templates from — no fewer, or every evaluation fails with Forbidden, and no
// more, since a permission nothing uses is one an operator has to justify.
//
// This is the assertion that makes the declaration a single source of truth
// rather than one more copy of the set. Every consumer in Go derives from it —
// `templates`, the controller's cache restrictions, the diagnosis sentence,
// the test fixtures — so shortening it changes all of them together and
// nothing they assert would notice. The chart is the one thing that cannot
// derive from it, which is exactly why it is the one that can fail.
func TestTheChartGrantsExactlyTheKindsTemplatesReads(t *testing.T) {
	chart, err := os.ReadFile(rbacdoc.ChartPath)
	if err != nil {
		t.Fatalf("reading the chart's RBAC: %v", err)
	}

	roles, err := rbacdoc.Roles(string(chart))
	if err != nil {
		t.Fatalf("reading the chart's rules: %v", err)
	}

	got := readGrants(t, rbacdoc.OfKind(roles, "ClusterRole"))
	want := wantedReads()

	if !slices.Equal(got, want) {
		t.Errorf("the chart grants read on\n\t%v\nbut binpack reads\n\t%v",
			got, want)
	}
}

// The reference is a hand-written copy of the chart, for operators who manage
// RBAC themselves rather than installing the chart's ClusterRole.
//
// A copy is what it has to be — the page explains each rule, and a generated
// one would explain nothing — so the thing worth checking is that it is still
// a copy. An RBAC reference that has drifted is worse than none: it is
// followed, and the install it produces fails on the first evaluation with a
// Forbidden naming a resource the page never mentioned.
func TestTheRBACReferenceGrantsWhatTheChartDoes(t *testing.T) {
	doc, err := os.ReadFile(rbacdoc.ReferencePath)
	if err != nil {
		t.Fatalf("reading the RBAC reference: %v", err)
	}

	roles, err := rbacdoc.Documented(string(doc))
	if err != nil {
		t.Fatalf("reading the RBAC reference's rules: %v", err)
	}

	got := readGrants(t, rbacdoc.OfKind(roles, "ClusterRole"))
	want := wantedReads()

	if !slices.Equal(got, want) {
		t.Errorf("docs/reference/rbac.md grants read on\n\t%v\nbut binpack reads\n\t%v",
			got, want)
	}
}

// The declaration has two halves in two packages, and they must cover the same
// kinds.
//
// [engine.TemplateKinds] says which owners binpack understands;
// [collect.TemplateSources] says how each is read. The split is forced —
// internal/engine may hold no client, and a client.ObjectList is a client —
// so what would otherwise be one literal is two, and this is what keeps them
// one set.
//
// Both directions, because they fail differently. A kind the engine names with
// no source here is caught at runtime by `templates`, loudly, but only on a
// cluster; a source for a kind the engine does not name is never read at all,
// and nothing would ever say so.
func TestEveryKindTheEngineNamesCanBeRead(t *testing.T) {
	named := map[engine.TemplateKind]bool{}
	for _, kind := range engine.TemplateKinds() {
		named[kind] = true
	}

	read := map[engine.TemplateKind]bool{}
	for _, src := range collect.TemplateSources() {
		read[src.TemplateKind] = true
	}

	for kind := range named {
		if !read[kind] {
			t.Errorf("the engine names %s %s and nothing here reads it",
				kind.APIVersion, kind.Kind)
		}
	}
	for kind := range read {
		if !named[kind] {
			t.Errorf("%s %s is read and the engine does not name it, so no pod is "+
				"ever matched to it", kind.APIVersion, kind.Kind)
		}
	}
}

// The reference pages restate the set too, and must restate the same one.
//
// Both explain something a generated list could not — what an operator should
// do about a pod binpack will not move, and what the unmodelled-node gauge is
// measuring — so they stay prose. This is what stops them becoming prose that
// is no longer true, which is the failure mode a reference page has: it is
// believed without being checked.
//
// The ADR that records *why* these four is deliberately not here. It is a
// dated decision rather than a description of the code, and the way to change
// what it says is to supersede it.
func TestTheReferenceDocsNameExactlyTheKindsThatAreRead(t *testing.T) {
	for _, page := range []struct{ path, phrase string }{
		// The diagnosis, repeated for the operator who reads the catalogue
		// rather than the command's output.
		{"../../docs/reference/diagnostics.md", readableKinds()},
		// A parenthetical beside binpack_nodes_unmodelled, which is the
		// measurement the list would be widened on the strength of — so a
		// stale list here misdescribes the evidence for changing it.
		{"../../docs/reference/metrics.md", "(" + strings.Join(kindNames(""), ", ") + ")"},
	} {
		doc, err := os.ReadFile(page.path)
		if err != nil {
			t.Fatalf("reading %s: %v", page.path, err)
		}
		if !strings.Contains(string(doc), page.phrase) {
			t.Errorf("%s does not name the readable kinds as %q", page.path, page.phrase)
		}
	}
}

// readableKinds is the set as running prose: "ReplicaSets, StatefulSets,
// DaemonSets and Jobs" — the same sentence the diagnosis catalogue renders.
//
// One phrase rather than a containment check per kind, because a test asking
// only whether each kind is mentioned passes when the set shrinks — a shorter
// list is trivially all mentioned — and would report a page that had gone
// stale as agreeing.
func readableKinds() string {
	plural := kindNames("s")
	return strings.Join(plural[:len(plural)-1], ", ") + " and " + plural[len(plural)-1]
}

// kindNames is every readable kind, each with the given suffix.
func kindNames(suffix string) []string {
	kinds := engine.TemplateKinds()
	out := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		out = append(out, kind.Kind+suffix)
	}
	return out
}

// Each source's closures must all be about the same kind.
//
// They are four independent references to one type in a table of four rows,
// which is the arrangement copy-paste gets wrong: a row whose List reads
// StatefulSets and whose Template reads a ReplicaSet's compiles and panics on
// the first cluster with a StatefulSet in it. Nothing else here would catch
// that — the tests above compare the table's *names* against the chart, and a
// misplaced closure leaves every name right.
func TestTemplateSourcesHandsBackACopy(t *testing.T) {
	// Callers range this list to know they have covered the set, and the list
	// is package state. A caller writing to an entry it was handed would
	// change the declaration for every other consumer in the process — the
	// same hazard as the read-only rule on snapshot objects, arrived at from
	// the other side. Nothing writes to one today, which is exactly why the
	// copy needs a test: it is a property with no symptom until it has one.
	first := collect.TemplateSources()
	if len(first) == 0 {
		t.Fatal("no template sources are declared")
	}
	was := first[0].Resource
	first[0].Resource = "mutated"

	if got := collect.TemplateSources()[0].Resource; got != was {
		t.Errorf("a caller's write reached the package's own declaration: %q", got)
	}
}

func TestEverySourcesClosuresAgreeOnItsKind(t *testing.T) {
	for _, src := range collect.TemplateSources() {
		obj := src.Object()

		if got := typeName(obj); got != src.Kind {
			t.Errorf("the %s source builds a %s", src.Kind, got)
		}
		if got, want := typeName(src.List()), src.Kind+"List"; got != want {
			t.Errorf("the %s source lists into a %s, want a %s", src.Kind, got, want)
		}
		if src.Template(obj) == nil {
			t.Errorf("the %s source reads no template", src.Kind)
		}
		if !src.ClearStatus(obj) {
			t.Errorf("the %s source does not recognise the object it builds", src.Kind)
		}
	}
}

// A kind in the core group must report no group at all.
//
// Its apiVersion is a bare version — "v1", not "core/v1" — so splitting on the
// separator and keeping the first half yields "v1", and an RBAC rule for
// apiGroups: ["v1"] grants nothing while looking entirely reasonable. Not
// hypothetical: ReplicationController is the kind operators most often ask for
// next, and it is in the core group.
func TestTheCoreGroupIsNamedByItsAbsence(t *testing.T) {
	for apiVersion, want := range map[string]string{
		"v1":        "",
		"apps/v1":   "apps",
		"batch/v1":  "batch",
		"batch/v1b": "batch",
	} {
		src := collect.TemplateSource{TemplateKind: engine.TemplateKind{APIVersion: apiVersion}}
		if got := src.Group(); got != want {
			t.Errorf("Group() of %q = %q, want %q", apiVersion, got, want)
		}
	}
}

// typeName is a Go type's bare name, without its package qualifier or pointer.
func typeName(v any) string {
	name := fmt.Sprintf("%T", v)
	return name[strings.LastIndex(name, ".")+1:]
}

// wantedReads is every resource binpack reads, in the form an RBAC rule names
// it.
//
// The controller kinds come from the declaration the whole read path is driven
// from. The other three are written out, because this test has to name them in
// order to subtract them — and naming them is what lets the assertion cover
// the *whole* ClusterRole rather than the two API groups the controller kinds
// happen to occupy today. A fifth kind in a third group would otherwise be
// granted somewhere the test was not looking.
//
// Reading something new therefore fails this test, in the same breath as it
// reminds whoever added the read that the chart has to grant it.
func wantedReads() []string {
	want := []string{"nodes", "pods", "poddisruptionbudgets.policy"}
	for _, src := range collect.TemplateSources() {
		want = append(want, grant(src.Group(), src.Resource))
	}
	slices.Sort(want)
	return want
}

// grant names a resource the way an RBAC rule and kubectl both do:
// resource.group, with the group left off for the core one.
func grant(group, resource string) string {
	if group == "" {
		return resource
	}
	return resource + "." + group
}

// readGrants is every resource the rules grant read on, sorted and deduplicated.
//
// Read means get, list and watch together: binpack's cache lists and then
// watches, and `explain` gets, so a rule short of any of the three is a rule
// that fails at runtime rather than a narrower version of the same permission.
// Filtering on it is also what keeps the mutating rules — nodes: patch,
// pods/eviction: create, the two events rules — out of the comparison without
// naming them.
func readGrants(t *testing.T, roles []rbacdoc.Role) []string {
	t.Helper()

	var out []string
	for _, role := range roles {
		for _, rule := range role.Rules {
			if !grantsRead(rule.Verbs) {
				continue
			}
			for _, group := range rule.APIGroups {
				for _, resource := range rule.Resources {
					out = append(out, grant(group, resource))
				}
			}
		}
	}

	slices.Sort(out)
	return slices.Compact(out)
}

// grantsRead reports whether verbs cover all three of get, list and watch.
func grantsRead(verbs []string) bool {
	for _, verb := range []string{"get", "list", "watch"} {
		if !slices.Contains(verbs, verb) {
			return false
		}
	}
	return true
}
