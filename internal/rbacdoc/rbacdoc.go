// Package rbacdoc reads binpack's RBAC out of the two documents that state
// it: the chart template that renders the roles, and the reference page an
// operator managing RBAC themselves writes a role from.
//
// It exists so that the tests holding chart, page and code to one another read
// through one implementation. There were four, each hand-rolled for its own
// question, and the loosest of them carried the chart-to-page comparison: it
// matched `apiGroups:`/`resources:`/`verbs:` line by line and pulled the values
// out of `[...]`, so a rule written as a block sequence — the ordinary
// Kubernetes idiom, and what a YAML formatter or a pasted upstream example
// produces — yielded nothing and was skipped in silence. Its consumers guarded
// against a parse that found *nothing*, which a parse that found less is not.
//
// So everything here parses YAML, and every way of finding nothing is an error
// rather than a smaller answer. What this must never do is see less than the
// documents hold.
//
// No dependency on testing, for the reason [mother] has none: this is ordinary
// code that tests happen to be the only callers of, and a package linked into
// the binary should not carry a test framework in to be sure of it.
package rbacdoc

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// The two documents, spelled once. The prefix is relative to a package
// directory one level under the repository root, which is where every caller
// lives.
const (
	ChartPath     = "../../charts/binpack/templates/rbac.yaml"
	ReferencePath = "../../docs/reference/rbac.md"
)

// Rule is one entry of a role's `rules`.
//
// Decoding into rbacv1.ClusterRole would be stricter and is not possible: the
// chart's metadata is Helm actions, and the placeholders they become are
// scalars where typed decoding wants maps.
//
// ResourceNames is here because dropping it made a grant look larger than it
// is. It restricts a request by the name in its path, so a rule carrying one
// authorises only requests that name that object — and a group/resource/verb
// triple read without it says a permission is granted that is not.
// NonResourceURLs is here for the opposite reason to ResourceNames: dropping
// it made a grant invisible rather than larger. A rule granting `get` on
// `nonResourceURLs: ["*"]` contributes no group/resource triple at all, so it
// passed every comparison and every count while the service account gained
// access to endpoints neither the code nor the page asks for.
type Rule struct {
	APIGroups       []string `json:"apiGroups"`
	Resources       []string `json:"resources"`
	ResourceNames   []string `json:"resourceNames"`
	NonResourceURLs []string `json:"nonResourceURLs"`
	Verbs           []string `json:"verbs"`
}

// Role is as much of a ClusterRole or Role as this package needs.
//
// It carries its Kind because the callers ask different questions of the two:
// what the cluster-wide role grants is not what a namespaced Role does.
//
// And its name, because "namespaced" is not one place. This chart renders two
// Roles into two different namespaces — one where the cluster-autoscaler
// publishes its status, one where binpack itself runs — so a rule moved from
// either to the other is granted somewhere binpack is not looking, and
// [Grants] cannot see the difference. The namespace itself cannot be compared:
// the chart writes both as Helm expressions and [HelmToYAML] renders them to
// the same placeholder. The name survives, because what distinguishes the two
// is a literal suffix on a templated prefix.
type Role struct {
	Kind     string   `json:"kind"`
	Metadata Metadata `json:"metadata"`
	Rules    []Rule   `json:"rules"`
}

// Metadata is the identifying half of a role, which for these documents means
// the name alone: the namespace is a Helm expression by the time this reads it.
type Metadata struct {
	Name string `json:"name"`
}

// Roles decodes every role a chart template declares, as the union of its
// branches.
//
// The union is the reading a permission audit wants: a rule the chart grants
// under some values is a rule the chart grants. [Section] is for the one
// question that needs a single branch.
func Roles(template string) ([]Role, error) {
	yml, err := HelmToYAML(template)
	if err != nil {
		return nil, err
	}
	return decode("the chart's RBAC", yml)
}

// Section is the roles inside one `{{- if <guard> }}` block of a chart
// template — the rules an operator gets only when they set that value.
//
// The block is a bare rule sequence rather than a document, so it is wrapped
// before decoding. Wrapped rather than matched line by line: what goes wrong
// here is a rule that stops being seen, and a decoder handed something it
// cannot read says so instead of returning less.
func Section(template, guard string) ([]Role, error) {
	block, _, err := section(template, guard)
	if err != nil {
		return nil, err
	}

	yml, err := HelmToYAML(block)
	if err != nil {
		return nil, err
	}
	return decode(guard+"'s rules", "rules:\n"+yml)
}

// Without is the template with one guarded block removed: what an install
// renders when that value is off.
//
// [Roles] is deliberately the union of every branch, which is the right
// reading of "what could this chart ever grant". It is the wrong reading of
// "what does an install that has not opted in grant", and a write binpack
// makes unconditionally has to be checked against the second — otherwise
// moving its rule inside an opt-in guard is invisible.
func Without(template, guard string) (string, error) {
	_, rest, err := section(template, guard)
	if err != nil {
		return "", err
	}
	return rest, nil
}

// section finds one guarded block, returning its body and the template with
// the whole block — guard, body and closing action — removed.
//
// The closing action is matched by depth rather than by taking the first
// `{{- end }}`, which is wrong the moment a guard contains another. It is
// already wrong: this chart nests the leader-election block inside
// `.Values.rbac.create`, so cutting at the first end returned twenty-five of
// the thirty-six pairs that file holds. Nothing asked it for that guard, which
// is the only reason it had not been noticed — and a reader that returns part
// of a permission set is the exact failure this package exists to refuse.
func section(template, guard string) (body, rest string, err error) {
	lines := strings.Split(template, "\n")

	start, depth := -1, 0
	for i, line := range lines {
		if start < 0 {
			if strings.Contains(line, "{{- if "+guard+" }}") {
				start, depth = i+1, depthDelta(line)
			}
			continue
		}

		if depth += depthDelta(line); depth == 0 {
			kept := append(append([]string{}, lines[:start-1]...), lines[i+1:]...)
			return strings.Join(lines[start:i], "\n"), strings.Join(kept, "\n"), nil
		}
	}

	if start < 0 {
		return "", "", fmt.Errorf("the chart no longer gates any rules on %s", guard)
	}
	return "", "", fmt.Errorf("the rules the chart gates on %s are not a closed block", guard)
}

// depthDelta is how far one line opens or closes template blocks.
//
// `template` is not here and `define`/`block` are: invoking a template opens
// nothing, while the other two are closed by an `{{- end }}` like `if` is.
func depthDelta(line string) int {
	delta := 0
	for _, action := range helmAction.FindAllString(line, -1) {
		body := strings.TrimSpace(strings.Trim(strings.Trim(action, "{}"), "-"))
		switch keyword, _, _ := strings.Cut(body, " "); keyword {
		case "if", "range", "with", "define", "block":
			delta++
		case "end":
			delta--
		}
	}
	return delta
}

// fencedYAML matches a ```yaml block in Markdown, capturing its body.
var fencedYAML = regexp.MustCompile("(?s)```yaml\n(.*?)```")

// Documented decodes the role snippets in a Markdown page.
//
// The snippets are fragments rather than manifests — they carry rules and no
// metadata, because the page is about what each rule is for — and some are
// bare rule sequences without even the `rules:` key, the section heading
// having already said which role they belong to. Both shapes are read.
//
// A block declaring no rules is a fragment quoted for discussion and is
// skipped; a block that mentions apiGroups and still yields none is this
// reader dropping a rule, and is an error. That distinction is the whole
// point of the package: silence about a rule is the failure being guarded
// against, so it may not be a way of succeeding.
func Documented(doc string) ([]Role, error) {
	var out []Role
	for _, match := range fencedYAML.FindAllStringSubmatch(doc, -1) {
		body := match[1]

		role, err := fragment(body)
		// A block naming apiGroups is a rule list, whatever became of it. Only
		// there is a failed or empty decode a dropped rule rather than a
		// snippet quoted for discussion, and there it is reported rather than
		// skipped.
		rules := strings.Contains(body, "apiGroups:")
		switch {
		case err != nil && rules:
			return nil, fmt.Errorf("a documented block declares apiGroups and does not "+
				"decode as rules, so this reader would drop it: %w\n%s", err, body)
		case len(role.Rules) == 0 && rules:
			return nil, fmt.Errorf("a documented block declares apiGroups and decoded "+
				"to no rules, so this reader would drop it:\n%s", body)
		case err != nil, len(role.Rules) == 0:
			continue
		}

		// Which object a fragment belongs to is written in its first line as a
		// comment, since it has no metadata to carry it. The whole line is
		// kept as the name: [Identity] matches the chart's metadata.name and
		// this against the same suffixes, and text is all the two documents
		// share.
		role.Kind, role.Metadata.Name = kindOf(body)
		out = append(out, role)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("the document states no rules at all")
	}
	return out, nil
}

// fragment decodes a documented snippet, which may be a document carrying
// `rules:` or the bare sequence under it.
//
// A partial decode is never returned. encoding/json fills in what it can and
// reports the first field it could not, so a page whose second rule has a
// malformed `resources:` decodes to two rules of which one grants nothing —
// and [Grants] emits nothing for it while this reports success. That is the
// same disappearance as the line reader's, arrived at from the other side, so
// an error here discards the value rather than qualifying it.
func fragment(body string) (Role, error) {
	var direct Role
	directErr := yaml.Unmarshal([]byte(body), &direct)
	if directErr == nil && len(direct.Rules) > 0 {
		return direct, nil
	}

	var wrapped Role
	if err := yaml.Unmarshal([]byte("rules:\n"+body), &wrapped); err != nil {
		if directErr != nil {
			return Role{}, fmt.Errorf("as a document: %w; as a rule sequence: %w", directErr, err)
		}
		return Role{}, err
	}
	return wrapped, nil
}

// kindOf reads the leading comment a documented fragment names its object in,
// returning the kind and the whole line to identify it by.
func kindOf(body string) (kind, name string) {
	first, _, _ := strings.Cut(body, "\n")
	first = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(first), "#"))
	switch {
	case strings.HasPrefix(first, "ClusterRole"):
		return "ClusterRole", first
	case strings.HasPrefix(first, "Role"):
		return "Role", first
	default:
		return "", ""
	}
}

// Grants is every `group/resource: verb` triple the roles hold.
//
// The core group is rendered as "core" rather than the empty string, so a
// failure message names something a reader can find in a role — and so that
// `events` from the core group and `events` from events.k8s.io stay different
// strings, which is the distinction a documented gap once hid in.
func Grants(roles []Role) map[string]bool {
	pairs := map[string]bool{}
	for _, role := range roles {
		for _, rule := range role.Rules {
			// A restricted grant is a different permission from the same
			// triple unrestricted, and keying them the same makes narrowing a
			// rule invisible to every comparison.
			restriction := ""
			if len(rule.ResourceNames) > 0 {
				restriction = " restricted to [" + strings.Join(rule.ResourceNames, ", ") + "]"
			}
			for _, group := range rule.APIGroups {
				if group == "" {
					group = "core"
				}
				for _, resource := range rule.Resources {
					for _, verb := range rule.Verbs {
						pairs[group+"/"+resource+": "+verb+restriction] = true
					}
				}
			}
			// Spelled as an endpoint rather than as a group and resource,
			// because that is what it is. What matters is that it appears at
			// all: a rule granting only these used to add nothing to this map.
			for _, url := range rule.NonResourceURLs {
				for _, verb := range rule.Verbs {
					pairs["nonResourceURL "+url+": "+verb] = true
				}
			}
		}
	}
	return pairs
}

// NamespacedRoles are the chart's namespaced Roles, by the suffix their names
// carry.
//
// A closed list, and checked as one: [Identity] returns "" for a Role matching
// none of these, and its callers fail on that rather than treating it as
// unplaceable-so-ignore. A third Role added to the chart therefore fails until
// it is named here, which is the point — the two documents this package reads
// have no shared vocabulary for these objects, so the correspondence has to be
// written down, and a correspondence that is written down and unenforced is
// how this whole review started.
var NamespacedRoles = []string{"-autoscaler-status", "-leader-election"}

// Identity is the scope a rule is granted at: the kind, and for a namespaced
// Role which of the chart's Roles it is.
//
// Kind alone is not enough. The chart renders two Roles into two different
// namespaces — one where the cluster-autoscaler publishes its status, one
// where binpack runs — so a rule moved between them is granted somewhere
// binpack never issues that request, and a comparison keyed on "Role" reads
// the two as interchangeable.
//
// The chart states the identity in metadata.name and the reference page in
// each block's leading comment; text is the only thing they share, so both are
// matched against the same suffixes.
func Identity(role Role) string {
	if role.Kind != "Role" {
		return role.Kind
	}
	for _, suffix := range NamespacedRoles {
		if strings.Contains(role.Metadata.Name, suffix) {
			return "Role" + suffix
		}
	}
	return ""
}

// namelessVerbs are the verbs whose requests carry no object name, so a
// resourceNames restriction on them authorises nothing at all.
//
// The chart's own RBAC comment establishes these two, and they are the reason
// it scopes the autoscaler-status Role by namespace rather than naming the
// object: "list and watch requests carry none — so a ClusterRole naming
// cluster-autoscaler-status would authorise `binpack explain`, which issues a
// get, and authorise nothing for the controller, whose cache issues a list
// followed by a watch". Other verbs may share the property; these are the two
// this repository has established and the two binpack's cache depends on.
var namelessVerbs = []string{"list", "watch"}

// Unauthorizable names grants that cannot authorise the request they appear
// to, however they are compared with anything else.
//
// This is the one property here that is not a comparison. Adding
// `resourceNames: [cluster-autoscaler-status]` to the ConfigMap rule leaves
// chart and page agreeing, every count above its floor and every scope check
// satisfied — and the controller's cache, which lists and then watches, holds
// no permission at all. A guard that only ever compares two documents cannot
// see a rule that is wrong on its own terms.
func Unauthorizable(roles []Role) []string {
	var out []string
	for _, role := range roles {
		for _, rule := range role.Rules {
			if len(rule.ResourceNames) == 0 {
				continue
			}
			for _, verb := range rule.Verbs {
				if !slices.Contains(namelessVerbs, verb) {
					continue
				}
				out = append(out, fmt.Sprintf("%s grants %v: %s on %v, restricted to %v",
					role.Metadata.Name, rule.APIGroups, verb, rule.Resources, rule.ResourceNames))
			}
		}
	}
	slices.Sort(out)
	return out
}

// OfKind is the roles of one kind, for a caller whose question is about the
// cluster-wide role and not the namespaced ones, or the other way round.
func OfKind(roles []Role, kind string) []Role {
	var out []Role
	for _, role := range roles {
		if role.Kind == kind {
			out = append(out, role)
		}
	}
	return out
}

// OfIdentity is the roles at one scope, which for a namespaced Role is finer
// than its kind. See [Identity].
func OfIdentity(roles []Role, identity string) []Role {
	var out []Role
	for _, role := range roles {
		if Identity(role) == identity {
			out = append(out, role)
		}
	}
	return out
}

// Identities are the scopes a rule can be granted at in this chart: cluster
// wide, or in one of the namespaced Roles.
func Identities() []string {
	out := []string{"ClusterRole"}
	for _, suffix := range NamespacedRoles {
		out = append(out, "Role"+suffix)
	}
	return out
}

// documentSeparator splits a multi-document YAML stream.
var documentSeparator = regexp.MustCompile(`(?m)^---$`)

// decode reads every role in a YAML stream, and every document that declares
// rules without saying what it is.
//
// A role is kept even when it grants nothing. Skipping it would be the reader
// disappearing again, one level up: a Role stripped of its last rule is not an
// absent Role, it is the object an install still creates and binds and which
// authorises nothing — and a caller counting the roles it found would report
// that as "I could not find them" rather than as "this one is empty".
//
// The rules-without-a-kind case is [Section]'s: a block lifted out of its
// document has no `kind:` to declare.
func decode(what, manifests string) ([]Role, error) {
	var out []Role
	for _, doc := range documentSeparator.Split(manifests, -1) {
		var role Role
		if err := yaml.Unmarshal([]byte(doc), &role); err != nil {
			return nil, fmt.Errorf("%s does not parse as YAML: %w\n%s", what, err, doc)
		}
		if role.Kind != "Role" && role.Kind != "ClusterRole" && len(role.Rules) == 0 {
			continue
		}
		out = append(out, role)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("%s declares no rules at all", what)
	}
	return out, nil
}

// helmAction matches one Go template action — the {{ ... }} a chart is written
// in.
var helmAction = regexp.MustCompile(`\{\{-?.*?-?\}\}`)

// helmControlKeywords are the actions that structure a template rather than
// producing a value.
var helmControlKeywords = []string{"if", "else", "end", "range", "with", "define", "block", "template"}

// HelmToYAML makes a chart template parseable, without running Helm.
//
// The templates do not parse as YAML — `{{ include "binpack.fullname" . }}` is
// not a YAML node — and rendering them properly means shelling out to a helm
// binary, which would put a second toolchain between `go test` and a green run
// for the sake of two RBAC rules.
//
// So this substitutes rather than renders, and the substitution is stated
// rather than done quietly. Control actions are deleted along with the line
// they sit on; every other action becomes a placeholder scalar. Deleting the
// conditionals means the result is the *union* of every branch, which is the
// reading a permission audit wants.
//
// What it cannot do is see less than the chart holds. An action this does not
// recognise leaves YAML that does not parse, and a control action sharing its
// line with content is refused outright rather than taking the content with
// it — so a template that outgrows this fails the test instead of quietly
// shrinking the set it is compared against.
func HelmToYAML(template string) (string, error) {
	const placeholder = "binpack-placeholder"

	var out []string
	for i, line := range strings.Split(template, "\n") {
		actions := helmAction.FindAllString(line, -1)
		if len(actions) == 0 {
			out = append(out, line)
			continue
		}

		if control := controlAction(actions); control != "" {
			if rest := strings.TrimSpace(helmAction.ReplaceAllString(line, "")); rest != "" {
				return "", fmt.Errorf("line %d puts the control action %q on a line with %q, "+
					"which this substitution would drop along with it — render the chart or "+
					"widen the substitution rather than letting a rule disappear",
					i+1, control, rest)
			}
			continue
		}

		out = append(out, helmAction.ReplaceAllString(line, placeholder))
	}
	return strings.Join(out, "\n"), nil
}

// controlAction returns the first action that structures the template, or "".
func controlAction(actions []string) string {
	for _, action := range actions {
		body := strings.TrimSpace(strings.Trim(strings.Trim(action, "{}"), "-"))
		keyword, _, _ := strings.Cut(body, " ")
		if slices.Contains(helmControlKeywords, keyword) {
			return action
		}
	}
	return ""
}
