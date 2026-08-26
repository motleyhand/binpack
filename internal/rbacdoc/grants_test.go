package rbacdoc

import (
	"maps"
	"slices"
	"testing"
)

// TestGrantsReadsResourceNamesAsASet holds a property no comparison in this
// repository can isolate.
//
// Every other guard here compares two documents, and a canonicalisation defect
// shows up in them as one missing permission and one surplus — indistinguishable
// from the chart and the page genuinely disagreeing. Reordering the same names
// on both sides at once is a change the other guards reject for their own
// reasons, so the property has to be asked of [Grants] directly.
//
// Kubernetes reads resourceNames as a set: the authorizer asks whether the
// requested name is among them, and neither order nor a repeat changes the
// answer. An operator who sorts that line to match their house style has
// changed no permission, and must not be told the two documents disagree.
func TestGrantsReadsResourceNamesAsASet(t *testing.T) {
	role := func(names ...string) []Role {
		return []Role{{Kind: "Role", Rules: []Rule{{
			APIGroups: []string{""}, Resources: []string{"configmaps"},
			Verbs: []string{"get"}, ResourceNames: names,
		}}}}
	}
	pairs := func(names ...string) []string {
		return slices.Sorted(maps.Keys(Grants(role(names...))))
	}

	if a, b := pairs("alpha", "beta"), pairs("beta", "alpha", "beta"); !slices.Equal(a, b) {
		t.Errorf("the same names in two orders produce different pairs, so a reordered "+
			"line reads as one permission missing and another added:\n\t%v\n\t%v", a, b)
	}

	// And the distinction the restriction exists to draw still holds: a
	// narrowed grant is a different permission from the same triple narrowed
	// differently, or not narrowed at all.
	if a, b := pairs("alpha", "beta"), pairs("alpha", "gamma"); slices.Equal(a, b) {
		t.Errorf("two different restrictions produce the same pair (%v), so narrowing a "+
			"rule is invisible to every comparison", a)
	}
	if a, b := pairs("alpha"), pairs(); slices.Equal(a, b) {
		t.Errorf("a restricted grant and an unrestricted one produce the same pair (%v)", a)
	}

	// Sorted on a copy: these rules come from objects the caller still holds,
	// and one Snapshot's pointers are shared by every other reader.
	original := []string{"beta", "alpha"}
	Grants(role(original...))
	if !slices.Equal(original, []string{"beta", "alpha"}) {
		t.Errorf("Grants reordered the caller's own slice: %v", original)
	}
}
