package rbacdoc

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// The identity an install has when nothing says otherwise. Named because the
// assertions compare against them: a Role's namespace is only checkable
// against the namespace the render was asked for.
const (
	DefaultRelease   = "binpack"
	DefaultNamespace = "binpack-system"
)

// ErrHelmMissing is helm not being on PATH.
//
// Distinguished from a chart that fails to render because the two want
// opposite responses. A chart that does not render is the defect these tests
// exist to catch; an absent helm means they did not run, and a test that did
// not run must say so in a way nobody reads as a pass.
var ErrHelmMissing = errors.New("helm is not on PATH")

// Options is one `helm template` invocation.
//
// Release and Namespace default rather than being required, because almost
// every caller wants the ordinary install and the two that do not are about
// the namespace specifically.
type Options struct {
	Release   string
	Namespace string

	// Set is `--set key=value`. Applied in sorted key order so that two
	// renders asking for the same values produce the same command, which is
	// what makes the cache below sound.
	Set map[string]string
}

func (o Options) args() []string {
	release, namespace := o.Release, o.Namespace
	if release == "" {
		release = DefaultRelease
	}
	if namespace == "" {
		namespace = DefaultNamespace
	}

	args := []string{"template", release, ChartDir, "--namespace", namespace}
	for _, key := range slices.Sorted(maps.Keys(o.Set)) {
		args = append(args, "--set", key+"="+o.Set[key])
	}
	return args
}

// rendered caches one invocation's output for the process.
//
// The chart does not change while a test binary runs, and the packages here
// render the same handful of value combinations from a dozen tests each. The
// key is the argument list, which [Options.args] makes canonical.
var rendered sync.Map

// Render is the chart as Helm renders it — the manifests an install applies.
//
// Helm rather than a substitution pass over the template, which is what this
// package did for its first thirty commits. Rendering a Helm template means
// evaluating Helm: `include`, `dig`, `default`, `toString`, whitespace
// chomping, and the difference between `:=` and `=` in a helper. Each of those
// was reimplemented here, each was reimplemented slightly wrong, and the
// resulting reader was several times the size of the chart it read — while
// still answering a question about a document Helm would render differently.
//
// The dependency this avoided was already present: CI installs helm and runs
// `helm template` in its own job, and the chart is the thing under test. So
// the substitution pass bought nothing and cost the accuracy that is the whole
// point of a guard.
//
// It also makes the value combinations first-class. Whether a rule appears
// only under `rbac.allowDraining` used to be answered by scanning the template
// for `{{- if }}` blocks and reading the enclosed text; it is now answered by
// rendering with the value on and with it off and comparing what comes out —
// which is the operator's question, and is right even for a guard written in a
// way no scanner anticipated.
func Render(opts Options) (string, error) {
	args := opts.args()

	// The chart's own bytes are part of the key, and reading them is not
	// optional bookkeeping.
	//
	// `go test` caches a package's result and decides whether the cache is
	// still good from the files the test binary opened — it cannot see what a
	// subprocess read. Helm reads the chart, so without this the chart is not
	// an input to any test that renders it: editing a template and running
	// `make test` returns the previous run's verdict, and every guard in this
	// package silently stops running the moment the thing it guards changes.
	// Reading the files here is what puts them in the test log.
	digest, err := chartDigest()
	if err != nil {
		return "", err
	}

	key := strings.Join(args, "\x00") + "\x00" + digest
	if out, ok := rendered.Load(key); ok {
		return out.(string), nil
	}

	if _, err := exec.LookPath("helm"); err != nil {
		return "", ErrHelmMissing
	}

	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("helm %s: %w\n%s", strings.Join(args, " "), err, out)
	}

	rendered.Store(key, string(out))
	return string(out), nil
}

// chartDigest is a hash of every file in the chart, and the act of reading
// them.
//
// Not cached: the read is the point, and a cached digest is a chart the test
// log stops naming. It is a few kilobytes off the page cache per render, and
// [rendered] keeps that to once per set of values.
func chartDigest() (string, error) {
	digest := fnv.New64a()
	err := filepath.WalkDir(ChartDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// The name as well as the contents, so a template renamed to one that
		// renders nothing is a different chart.
		_, _ = digest.Write([]byte(path))
		_, _ = digest.Write(body)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("reading the chart at %s: %w", ChartDir, err)
	}
	return fmt.Sprintf("%016x", digest.Sum64()), nil
}

// MustRender is [Render] for a caller that has no way to proceed without it.
//
// It reports through the fatal function it is handed rather than taking a
// *testing.T, for the reason the package holds no other testing import: this
// is ordinary code, and a package linked into the binary should not carry a
// test framework in.
//
// An absent helm fails here rather than skipping. A skipped comparison between
// the chart and the reference page is a comparison nobody is making, and it
// reads in a test log exactly like one that had nothing to report — which is
// the failure mode this whole package exists to close.
func MustRender(fatal func(args ...any), opts Options) string {
	out, err := Render(opts)
	switch {
	case errors.Is(err, ErrHelmMissing):
		fatal("helm is not on PATH, so the chart could not be rendered and none of the " +
			"guards holding it to the reference page ran. Install helm — the chart is " +
			"what these tests are about, and skipping them silently would leave the " +
			"comparison unmade: https://helm.sh/docs/intro/install/")
	case err != nil:
		fatal(err)
	}
	return out
}
