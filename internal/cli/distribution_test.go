package cli

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The artefacts a release is built from, and the two files a stranger looks
// for when they want to report something or find out what changed.
//
// Asserted from this package for the reason capability_doc_test.go gives: it
// already reads the repository's own files, and a test about the repository's
// layout needs a package that compiles without a cluster. Nothing here imports
// anything; these are file assertions, and their subject is the tree.
const (
	dockerfilePath  = "../../Dockerfile"
	dependabotPath  = "../../.github/dependabot.yml"
	ciWorkflowPath  = "../../.github/workflows/ci.yaml"
	relWorkflowPath = "../../.github/workflows/release.yaml"
	makefilePath    = "../../Makefile"
	contributing    = "../../CONTRIBUTING.md"
	changelogPath   = "../../CHANGELOG.md"
	securityPolicy  = "../../SECURITY.md"
	mainGoMod       = "../../go.mod"
	harnessGoMod    = "../../test/differential/go.mod"
	versioningRef   = "../../docs/reference/versioning.md"
	readmePath      = "../../README.md"
	workflowDir     = "../../.github/workflows"
)

func readRepoFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

// TestTheBaseImageIsPinnedByDigest holds the two halves of one change
// together, because either alone is worse than neither.
//
// A digest makes the base a release shipped on recoverable from the source
// tree, and it is what stops a rebuilt or rotated upstream tag entering a
// release with no diff and no review. What it costs is the automatic patching
// the floating tag was providing: distroless/static is not empty — it carries
// a CA bundle and tzdata, and those do get rebuilt — so a pin with nothing
// watching it freezes the base until somebody remembers.
//
// The `docker` ecosystem entry is what pays that back, as a reviewable pull
// request rather than a silent pull. It is specifically the tag beside the
// digest that makes it work: dependabot's Docker parser captures `tag` and
// `digest` separately from a FROM line (dependabot-core,
// docker/lib/dependabot/docker/file_parser.rb, FROM_LINE), and its
// digest-only suppression applies only to a `comparable?` — that is, version —
// tag (update_checker.rb, digest_only_update_suppressed?). `nonroot` is not
// one, so a digest that moves under the same tag is still proposed.
func TestTheBaseImageIsPinnedByDigest(t *testing.T) {
	var from string
	for line := range strings.SplitSeq(readRepoFile(t, dockerfilePath), "\n") {
		if strings.HasPrefix(line, "FROM ") {
			from = strings.TrimSpace(strings.TrimPrefix(line, "FROM "))
			break
		}
	}
	if from == "" {
		t.Fatal("the Dockerfile has no FROM line; this test would assert nothing")
	}

	if !regexp.MustCompile(`@sha256:[0-9a-f]{64}$`).MatchString(from) {
		t.Errorf("the base image is %q, which names no digest: two builds of the "+
			"same tag can differ, and the base a release shipped on is not "+
			"recoverable from this tree", from)
	}

	// The tag as well as the digest. A bare `image@sha256:…` is reproducible
	// and unreadable, and dependabot resolves a tagless pin against `latest`
	// rather than against the tag that was meant — which for this image is not
	// `latest` at all.
	if !regexp.MustCompile(`:[\w][\w.-]*@sha256:`).MatchString(from) {
		t.Errorf("the base image is %q, which pins a digest without the tag it "+
			"came from; keep `image:tag@sha256:…` so the pin records what it "+
			"was pinned from and dependabot has a tag to resolve", from)
	}

	dependabot := readRepoFile(t, dependabotPath)
	if !strings.Contains(dependabot, "package-ecosystem: docker") {
		t.Error("dependabot has no docker ecosystem, so the digest above is now " +
			"frozen: the distroless static base carries a CA bundle and tzdata " +
			"and is rebuilt without the tag moving")
	}
}

// linterPin reads the version each of the four places names, keyed by the file
// it came from. Absence is reported as the empty string rather than skipped,
// so a file that stops naming a version fails rather than dropping out of the
// comparison.
func linterPin(t *testing.T) map[string]string {
	t.Helper()

	// The `version:` input of the golangci-lint-action block, not the action's
	// own major: `uses: …@v9` and `with: version: v2.12.2` are different
	// numbers and only the second decides which linter runs.
	action := regexp.MustCompile(`golangci/golangci-lint-action@v\d+\s*\n\s*with:\s*\n\s*version: (v\d+(?:\.\d+)*)`)
	found := map[string]string{}
	for _, path := range []string{ciWorkflowPath, relWorkflowPath} {
		if m := action.FindStringSubmatch(readRepoFile(t, path)); m != nil {
			found[path] = m[1]
		} else {
			found[path] = ""
		}
	}

	// The Makefile's own copy. `make lint` runs whatever golangci-lint is on
	// PATH — on the runners that is the binary the action seeded, and locally
	// it is whatever the contributor installed — so the number here is a
	// declaration of what the project expects, and the target compares the
	// installed binary against it.
	if m := regexp.MustCompile(`(?m)^GOLANGCI_VERSION\s*[:?]?=\s*(v\d+(?:\.\d+)*)\s*$`).
		FindStringSubmatch(readRepoFile(t, makefilePath)); m != nil {
		found[makefilePath] = m[1]
	} else {
		found[makefilePath] = ""
	}

	// The first version-shaped string after CONTRIBUTING.md first mentions the
	// linter. Not a same-line match: the sentence is prose and wraps, and a
	// guard that a rewrap can switch off is not a guard.
	doc := readRepoFile(t, contributing)
	found[contributing] = ""
	if at := strings.Index(doc, "golangci-lint"); at >= 0 {
		found[contributing] = regexp.MustCompile(`v\d+\.\d+\.\d+`).FindString(doc[at:])
	}
	return found
}

// TestTheLinterVersionAgreesEverywhere mechanises the invariant release.yaml
// states three lines above the step it governs: "Pinned to the same version: a
// release that fails on a lint rule the pull request never ran is a release
// blocked by a disagreement between two workflows."
//
// Nothing held it. hack/check-workflows.py compares the two workflows' setup
// actions with the version split off and discarded, so ci.yaml at v2.13.0 and
// release.yaml at v2.12.2 exits 0 — and so does @v10 against @v9. That half
// now lives in the script, which is where the repository keeps cross-workflow
// assertions; what is here is the half the script cannot see, since the
// Makefile is not a workflow.
//
// Four places, not three. CONTRIBUTING.md said "v2", a range spanning every
// release from v2.0 to v2.13 and beyond, which is how a contributor ends up
// linting with a different build from the one that will judge their pull
// request. That is not hypothetical: the machine this was written on had
// 2.13.0 installed against a pinned v2.12.2.
func TestTheLinterVersionAgreesEverywhere(t *testing.T) {
	pinned := linterPin(t)

	want := pinned[ciWorkflowPath]
	if want == "" {
		t.Fatal("ci.yaml names no golangci-lint version; this test would assert nothing")
	}
	for path, got := range pinned {
		if got == "" {
			t.Errorf("%s names no golangci-lint version, so it cannot be held to one", path)
			continue
		}
		if got != want {
			t.Errorf("%s pins golangci-lint %s and ci.yaml pins %s: whichever runs "+
				"second reports rules the first never applied", path, got, want)
		}
	}
}

// TestTheProjectCanReceiveAndDetectVulnerabilityReports covers the two
// directions a vulnerability arrives from, and pins the one place govulncheck
// must not be.
//
// Inwards: binpack is distributed as an image and an OCI chart, and holds
// `nodes: patch` and `pods/eviction: create` on other people's clusters. A
// researcher with no private channel reports what they find as a public issue,
// which is a disclosure with no embargo. SECURITY.md is not itself the
// channel — GitHub's private vulnerability reporting is a repository setting
// and no file can turn it on — so what this asserts is that the file exists
// and names where to go.
//
// Outwards: govulncheck adds reachability over the Dependabot alerts the
// repository already receives, and covers the standard library and toolchain
// that the dependency graph does not carry.
//
// And it must stay out of `make check`. release.yaml runs `make check` on
// every tag, and govulncheck's verdict is a function of the advisory database
// at the moment it runs rather than of the tree — so the identical commit
// passes on the pull request and fails on the tag once somebody else's CVE
// lands. That is the failure release.yaml's own comment argues against, and it
// would also hand every contributor a `make check` they cannot get green for
// reasons that have nothing to do with their change. A scheduled job carries
// the same information and blocks nobody.
func TestTheProjectCanReceiveAndDetectVulnerabilityReports(t *testing.T) {
	policy := readRepoFile(t, securityPolicy)
	if !strings.Contains(policy, "security/advisories") {
		t.Error("SECURITY.md does not point at the repository's advisory page, so it " +
			"names no private channel and a report arrives as a public issue")
	}

	makefile := readRepoFile(t, makefilePath)
	if !strings.Contains(makefile, "govulncheck") {
		t.Error("no Makefile target runs govulncheck, so nothing in the tree looks " +
			"for a known-vulnerable dependency or an advisory against the toolchain")
	}

	// The prerequisites of `check`, as written. A recursive walk would be
	// stricter than the claim: what matters is that a tag does not run a
	// time-dependent check, and `check` is what a tag runs.
	check := regexp.MustCompile(`(?m)^check:([^#\n]*)`).FindStringSubmatch(makefile)
	if check == nil {
		t.Fatal("the Makefile has no check target; this test would assert nothing")
	}
	for _, prerequisite := range strings.Fields(check[1]) {
		if prerequisite == "vuln" {
			t.Error("`make check` depends on the vulnerability scan, and release.yaml " +
				"runs `make check` on every tag: a CVE published against a dependency " +
				"would then block a release built from a commit that already passed")
		}
	}

	// Run by something, or it is a target nobody invokes. A schedule is the
	// trigger that matters: an advisory published against code that has not
	// changed is invisible to any event the repository produces.
	scheduled := false
	entries, err := os.ReadDir(workflowDir)
	if err != nil {
		t.Fatalf("reading the workflows: %v", err)
	}
	for _, entry := range entries {
		workflow := readRepoFile(t, workflowDir+"/"+entry.Name())
		if strings.Contains(workflow, "govulncheck") && strings.Contains(workflow, "schedule:") {
			scheduled = true
		}
	}
	if !scheduled {
		t.Error("no workflow runs govulncheck on a schedule, so an advisory published " +
			"against unchanged code is never noticed")
	}
}

// changelogHeadings returns the released versions the changelog carries, in
// the order they appear.
func changelogHeadings(doc string) []string {
	var versions []string
	for _, m := range regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\]`).FindAllStringSubmatch(doc, -1) {
		versions = append(versions, m[1])
	}
	return versions
}

// TestTheChangelogVersioningPromisesExists resolves a dangling reference in
// the document that defines the compatibility contract.
//
// versioning.md uses "the changelog" twice as a definite noun and as the thing
// the promise rests on: a minor bump may break a public name and "the
// changelog says which", and a deprecated name is "listed in the changelog
// under a heading that says so". No such artefact existed — no CHANGELOG.md,
// no `.github/release.yml`, no `changelog.groups` in .goreleaser.yaml, and no
// entry in the README's table of contents. A reader asking which public name
// broke in a minor release was told where the answer was and given no path.
//
// So this asserts the promise rather than the prose: the file exists, both
// documents reach it, every released section says something, and there is an
// Unreleased section for the next break to land in *before* its tag. That last
// one is the difference between a record and a reconstruction — a changelog
// written at release time is written from the commit log, which is exactly
// where the deprecation heading is not.
func TestTheChangelogVersioningPromisesExists(t *testing.T) {
	doc := readRepoFile(t, changelogPath)

	if !strings.Contains(doc, "## [Unreleased]") {
		t.Error("the changelog has no Unreleased section, so a deprecation has " +
			"nowhere to be listed until somebody cuts a release")
	}

	// The heading vocabulary versioning.md promises by name.
	if !strings.Contains(doc, "Deprecated") {
		t.Error("the changelog never uses a Deprecated heading, which is the one " +
			"docs/reference/versioning.md promises a going-away name is listed under")
	}

	released := changelogHeadings(doc)
	if len(released) == 0 {
		t.Fatal("the changelog records no released version; a changelog that starts " +
			"at the next release does not answer what changed in the last one")
	}

	// Every version heading is reachable as a diff, and every link definition
	// belongs to a heading. Both directions: a version added without a link
	// leaves a reader with a number and nowhere to go, and a link left behind
	// by a deleted section is a claim about a release that is not described.
	linked := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\[(\d+\.\d+\.\d+)\]: `).FindAllStringSubmatch(doc, -1) {
		linked[m[1]] = true
	}
	for _, version := range released {
		if !linked[version] {
			t.Errorf("the changelog describes %s and defines no link for it", version)
		}
		delete(linked, version)
	}
	for version := range linked {
		t.Errorf("the changelog links %s and has no section describing it", version)
	}

	// Not a stub. Each released section carries at least one Keep-a-Changelog
	// subsection, so backfilling cannot be satisfied by a heading and a date.
	for i, part := range strings.Split(doc, "\n## ")[1:] {
		if strings.HasPrefix(part, "[Unreleased]") {
			continue
		}
		if !strings.Contains(part, "\n### ") {
			heading, _, _ := strings.Cut(part, "\n")
			t.Errorf("changelog section %d (%q) has no subsections, so it records a "+
				"version without recording what changed in it", i, heading)
		}
	}

	// And the reference resolves from both ends: the contract points at the
	// record, and the reader who finds the record can get back to the contract
	// that says what a break means.
	if !strings.Contains(readRepoFile(t, versioningRef), "CHANGELOG.md") {
		t.Error("docs/reference/versioning.md still says `the changelog` without " +
			"naming the file, which is the dangling reference this closes")
	}
	if !strings.Contains(readRepoFile(t, readmePath), "CHANGELOG.md") {
		t.Error("the README's table of contents does not list the changelog, and it " +
			"lists every other document")
	}
}

// TestContributingStatesTheGoVersionGoModRequires is a drift guard rather than
// a correction: CONTRIBUTING.md and go.mod agree today, and nothing was
// holding them there.
//
// The drift is invisible where it is introduced. Every setup-go step in both
// workflows uses `go-version-file: go.mod`, so CI runs whatever go.mod says
// and never reads CONTRIBUTING.md; and under the default GOTOOLCHAIN=auto a
// contributor on an older Go downloads the newer toolchain and never sees a
// problem either. It surfaces only where GOTOOLCHAIN is `local` or pinned —
// distribution packages that suppress toolchain downloads, and corporate
// environments — as `go: go.mod requires go >= …` on the first command the
// file gives them.
//
// Both modules, and the exact string. `1.26` would satisfy a prefix match
// against `1.26.0` while understating the floor the moment the directive
// gains a patch component, which is the only way this can go wrong quietly.
func TestContributingStatesTheGoVersionGoModRequires(t *testing.T) {
	directive := regexp.MustCompile(`(?m)^go (\S+)$`)

	required := map[string]string{}
	for _, path := range []string{mainGoMod, harnessGoMod} {
		m := directive.FindStringSubmatch(readRepoFile(t, path))
		if m == nil {
			t.Fatalf("%s has no go directive; this test would assert nothing", path)
		}
		required[path] = m[1]
	}
	if required[mainGoMod] != required[harnessGoMod] {
		t.Errorf("go.mod requires Go %s and the differential harness requires %s, so "+
			"one sentence in CONTRIBUTING.md cannot state the floor for both",
			required[mainGoMod], required[harnessGoMod])
	}

	if doc := readRepoFile(t, contributing); !strings.Contains(doc, required[mainGoMod]) {
		t.Errorf("go.mod requires Go %s and CONTRIBUTING.md does not say so; it is "+
			"the only place in the repository that states a Go version, and CI "+
			"reads the version from go.mod so it can never notice",
			required[mainGoMod])
	}
}
