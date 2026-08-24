package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/motleyhand/binpack/api/v1alpha1"
	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/mother"
	"github.com/motleyhand/binpack/internal/version"
)

const referenceDir = "../../docs/reference"

// TestEveryJSONFieldIsDocumented holds the `--output json` surface to the
// promise docs/reference/versioning.md makes about it.
//
// That page lists "`--output json` field names" among the eight public
// surfaces, and says in the next breath that the prose beside them is not
// public — "which is exactly why every one of them has a stable code beside
// it". A field name that is public and documented nowhere is a promise about
// something a reader cannot look up, and there were twenty-odd of them.
//
// Reflection rather than a list, for the reason every other guard here uses an
// enumerator: a list of a struct's fields is a copy of the struct that stops
// agreeing with it the first time somebody adds one.
func TestEveryJSONFieldIsDocumented(t *testing.T) {
	documented := documentedNames(referenceCorpus(t))

	for _, subject := range []struct {
		what string
		typ  reflect.Type
	}{
		{"binpack explain", reflect.TypeOf(explainView{})},
		{"binpack diagnose", reflect.TypeOf(findingView{})},
		{"binpack config validate", reflect.TypeOf(resolvedConfig{})},
		{"binpack version", reflect.TypeOf(version.Info{})},
	} {
		for _, name := range jsonFieldNames(subject.typ) {
			if !documented[name] {
				t.Errorf("%s --output json emits %q, and no reference page names it",
					subject.what, name)
			}
		}
	}
}

// documentedNames is every field name a reference page mentions.
//
// A backticked token counts for every segment of its path, because that is how
// the reference reads best: `nodes[].pool` says more to somebody looking a
// field up than `pool` does, and it documents the enclosing `nodes` array at
// the same time. What this asks is whether a field appears at all, not where
// or under what heading — a page that names it in a path has somewhere to send
// a reader, which is the whole of the promise versioning.md makes about it.
func documentedNames(reference string) map[string]bool {
	out := map[string]bool{}
	for token := range strings.SplitSeq(reference, "`") {
		for _, segment := range strings.Split(strings.ReplaceAll(token, "[]", ""), ".") {
			out[segment] = true
		}
	}
	return out
}

// jsonFieldNames is every json tag a value of typ can carry, including those
// of the structs nested inside it.
func jsonFieldNames(typ reflect.Type) []string {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return nil
	}

	var out []string
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		switch tag {
		case "-":
			continue
		case "":
			// An embedded struct with no tag of its own contributes its
			// fields to the enclosing object, so its names are the ones a
			// consumer sees.
		default:
			out = append(out, tag)
		}
		out = append(out, jsonFieldNames(field.Type)...)
	}
	return out
}

func referenceCorpus(t *testing.T) string {
	t.Helper()

	entries, err := os.ReadDir(referenceDir)
	if err != nil {
		t.Fatalf("reading %s: %v", referenceDir, err)
	}

	var b strings.Builder
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(referenceDir, entry.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", entry.Name(), err)
		}
		b.Write(data)
	}
	if b.Len() == 0 {
		t.Fatalf("no reference pages found under %s", referenceDir)
	}
	return b.String()
}

// TestExplainJSONAlwaysEmitsArrays settles a contract two commands in one
// binary disagreed about.
//
// `binpack diagnose` builds its array with make() under a comment saying why —
// "so a healthy cluster yields [] and needs no special handling downstream" —
// and a test pinning it. `explain` appended to nil slices, so `jq '.nodes[]'`
// worked on every cluster it was developed against and raised "Cannot iterate
// over null" on the one state it most needs reporting: no autoscaler, or one
// whose status went stale, which is a five-minute window on every
// cluster-autoscaler restart.
func TestExplainJSONAlwaysEmitsArrays(t *testing.T) {
	s := explainCluster(nil, nil)
	s.Autoscaler = engine.Autoscaler{}

	var doc map[string]any
	if err := json.Unmarshal([]byte(renderedExplain(t, outputJSON, s, explainConfig())), &doc); err != nil {
		t.Fatalf("decoding the JSON output: %v", err)
	}

	autoscaler, ok := doc["autoscaler"].(map[string]any)
	if !ok {
		t.Fatalf("no autoscaler object in %v", doc)
	}
	for what, got := range map[string]any{
		"nodes":            doc["nodes"],
		"autoscaler.pools": autoscaler["pools"],
	} {
		if got == nil {
			t.Errorf("%s is null on a cluster with no autoscaler; every consumer "+
				"iterating it has to special-case the state binpack most needs to report",
				what)
			continue
		}
		if _, ok := got.([]any); !ok {
			t.Errorf("%s is %T, want an array", what, got)
		}
	}
}

// TestExplainJSONCarriesACodeBesideEveryRefusal is versioning.md's own
// justification, applied to the two verdicts that most need it.
//
// "Prose is not public … which is exactly why every one of them has a stable
// code beside it" is true of a skipped node and false of a blocked or
// infeasible one: both carried sentences alone. The codes existed the whole
// time — seven in internal/fit, eight in the engine — and reached no metric,
// no log field and no JSON key, so ADR-0006's promise that the allowlist
// widens "on evidence" had no measurement behind it.
func TestExplainJSONCarriesACodeBesideEveryRefusal(t *testing.T) {
	// One pod that cannot be placed anywhere, because every destination
	// carries a taint it does not tolerate.
	tainted := func(name string) *corev1.Node {
		return explainNode(name, mother.Tainted("dedicated", "db", corev1.TaintEffectNoSchedule))
	}
	s := explainCluster(
		[]*corev1.Node{explainNode("node-a"), tainted("node-b"), tainted("node-c")},
		[]*corev1.Pod{mother.Pod("default", "web", mother.OnNode("node-a"))})

	var view explainView
	if err := json.Unmarshal([]byte(renderedExplain(t, outputJSON, s, explainConfig())), &view); err != nil {
		t.Fatalf("decoding the JSON output: %v", err)
	}

	report := nodeNamed(t, view, "node-a")
	if report.Verdict != engine.VerdictInfeasible {
		t.Fatalf("node-a is %s, want infeasible", report.Verdict)
	}
	if len(report.Refusals) == 0 {
		t.Fatalf("no refusals recorded for node-a")
	}
	for dest, refusal := range report.Refusals {
		if refusal.Code == "" {
			t.Errorf("%s refused with %q and no code: a consumer has nothing to "+
				"branch on but a sentence the docs reserve the right to reword",
				dest, refusal.Message)
		}
		if refusal.Message == "" {
			t.Errorf("%s refused with code %q and no message", dest, refusal.Code)
		}
	}
}

// TestExplainJSONJoinsNodesToPools closes the gap between the two halves of
// one document.
//
// `autoscaler.pools[]` identified a pool only by the provider's node-group
// identifier and `nodes[]` only by the readable label value, so on the default
// DOKS configuration the report printed a UUID above and "pool-4g" below with
// no field connecting them. Both spellings were already computed; each half
// published one of them.
func TestExplainJSONJoinsNodesToPools(t *testing.T) {
	s := explainCluster([]*corev1.Node{explainNode("node-a"), explainNode("node-b")}, nil)

	var view explainView
	if err := json.Unmarshal([]byte(renderedExplain(t, outputJSON, s, namedPools())), &view); err != nil {
		t.Fatalf("decoding the JSON output: %v", err)
	}

	for _, r := range view.Nodes {
		if r.Group == "" {
			t.Fatalf("%s carries no group, so nothing joins it to a pool", r.Name)
		}
		found := false
		for _, p := range view.Autoscaler.Pools {
			if p.ID == r.Group {
				found = true
				if p.Name == "" {
					t.Errorf("pool %s carries no readable name, though its nodes do", p.ID)
				}
			}
		}
		if !found {
			t.Errorf("%s is in group %q and no reported pool has that id", r.Name, r.Group)
		}
	}
}

// namedPools is explainConfig pointed at the label the fixtures actually
// carry, which is the DOKS-shaped cluster: both the identifier and a readable
// pool name on every node.
func namedPools() engine.Config {
	cfg := explainConfig()
	cfg.PoolNameLabel = "doks.digitalocean.com/node-pool"
	return cfg
}

// TestExplainJSONNamesThePoolWhenOnlyTheIdentifierIsPresent is the explain
// half of SSOT-02.
//
// `nodes[].pool` was the raw label value and nothing more, so on every cluster
// whose provider publishes no readable pool name the field was omitted
// entirely — while binpack_pool_nodes and `binpack diagnose`, looking at the
// same cluster in the same evaluation, named that pool by its identifier.
// Three spellings of one pool, one of them absent.
func TestExplainJSONNamesThePoolWhenOnlyTheIdentifierIsPresent(t *testing.T) {
	s := explainCluster([]*corev1.Node{explainNode("node-a"), explainNode("node-b")}, nil)

	var view explainView
	if err := json.Unmarshal([]byte(renderedExplain(t, outputJSON, s, explainConfig())), &view); err != nil {
		t.Fatalf("decoding the JSON output: %v", err)
	}

	// explainConfig names a pool-name label the fixtures do not carry, which
	// is EKS, AKS and most GKE installs.
	r := nodeNamed(t, view, "node-a")
	if r.Pool != explainPoolID {
		t.Errorf("nodes[].pool = %q, want the identifier %q: with no readable name "+
			"there is nothing else to call the pool, and omitting the field leaves "+
			"the report with no pool for this node at all", r.Pool, explainPoolID)
	}
}

func nodeNamed(t *testing.T, view explainView, name string) nodeReport {
	t.Helper()
	for _, r := range view.Nodes {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("%s is not in the report", name)
	return nodeReport{}
}

// TestValidateJSONRoundTrips is PUBLIC-05, expressed as the property that
// actually matters.
//
// `binpack config validate --output json` spelled the policy in a vocabulary
// that existed nowhere else — `defaultPolicy.backoffInitial` where the
// document says `policy.backoff.initial` — while spelling `interval`,
// `dryRun` and the whole `discovery` object exactly as the document does. That
// asymmetry is what makes it an accident rather than a convention: the
// discovery half matched because the view embeds the API type, and the policy
// half diverged because it mirrors the flattened *resolved* struct.
//
// The assertion is loadability rather than equality of documents. The report
// is deliberately the effective settings, so reloading it pins every default
// and every inherited pool value as an explicit override — a different
// document that must describe the same binpack.
func TestValidateJSONRoundTrips(t *testing.T) {
	const document = `apiVersion: binpack.motleyhand.com/v1alpha1
kind: BinpackConfig
interval: 3m
policy:
  feasibility:
    expendablePriorityCutoff: -20
  backoff:
    initial: 45m
  exclusions:
    namespaces: [kube-system]
pools:
  - name: pool-4g
    drain:
      maxPodsPerDrain: 5
`

	cfg, err := v1alpha1.Load([]byte(document))
	if err != nil {
		t.Fatalf("loading the source document: %v", err)
	}

	encoded, err := json.Marshal(resolve(cfg))
	if err != nil {
		t.Fatalf("encoding the resolved configuration: %v", err)
	}

	reloaded, err := v1alpha1.Load(encoded)
	if err != nil {
		t.Fatalf("the resolved report does not load as a configuration document, so it "+
			"publishes a second spelling of every policy field: %v\n%s", err, encoded)
	}

	// Same binpack, not merely a document that parses: every resolved value
	// has to survive the trip, including the pool override.
	for _, pool := range []string{"", "pool-4g"} {
		if got, want := reloaded.PolicyFor(pool), cfg.PolicyFor(pool); !reflect.DeepEqual(got, want) {
			t.Errorf("policy for %q resolves differently after a round trip:\n got %+v\nwant %+v",
				pool, got, want)
		}
	}
}
