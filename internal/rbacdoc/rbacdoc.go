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
type Rule struct {
	APIGroups []string `json:"apiGroups"`
	Resources []string `json:"resources"`
	Verbs     []string `json:"verbs"`
}

// Role is as much of a ClusterRole or Role as this package needs.
//
// It carries its Kind because the callers ask different questions of the two:
// what the cluster-wide role grants is not what a namespaced Role does.
type Role struct {
	Kind  string `json:"kind"`
	Rules []Rule `json:"rules"`
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
	_, rest, ok := strings.Cut(template, "{{- if "+guard+" }}")
	if !ok {
		return nil, fmt.Errorf("the chart no longer gates any rules on %s", guard)
	}
	block, _, ok := strings.Cut(rest, "{{- end }}")
	if !ok {
		return nil, fmt.Errorf("the rules the chart gates on %s are not a closed block", guard)
	}

	yml, err := HelmToYAML(block)
	if err != nil {
		return nil, err
	}
	return decode(guard+"'s rules", "rules:\n"+yml)
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

		var role Role
		if err := yaml.Unmarshal([]byte(body), &role); err != nil || len(role.Rules) == 0 {
			role = Role{}
			_ = yaml.Unmarshal([]byte("rules:\n"+body), &role)
		}

		if len(role.Rules) == 0 {
			if strings.Contains(body, "apiGroups:") {
				return nil, fmt.Errorf("a documented block declares apiGroups and decoded "+
					"to no rules, so this reader would drop it:\n%s", body)
			}
			continue
		}

		// Which object a fragment belongs to is written in its first line as a
		// comment, since it has no metadata to carry it.
		role.Kind = kindOf(body)
		out = append(out, role)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("the document states no rules at all")
	}
	return out, nil
}

// kindOf reads the leading comment a documented fragment names its object in.
func kindOf(body string) string {
	first, _, _ := strings.Cut(body, "\n")
	first = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(first), "#"))
	switch {
	case strings.HasPrefix(first, "ClusterRole"):
		return "ClusterRole"
	case strings.HasPrefix(first, "Role"):
		return "Role"
	default:
		return ""
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
			for _, group := range rule.APIGroups {
				if group == "" {
					group = "core"
				}
				for _, resource := range rule.Resources {
					for _, verb := range rule.Verbs {
						pairs[group+"/"+resource+": "+verb] = true
					}
				}
			}
		}
	}
	return pairs
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

// documentSeparator splits a multi-document YAML stream.
var documentSeparator = regexp.MustCompile(`(?m)^---$`)

// decode reads every document of a YAML stream that declares rules.
func decode(what, manifests string) ([]Role, error) {
	var out []Role
	for _, doc := range documentSeparator.Split(manifests, -1) {
		var role Role
		if err := yaml.Unmarshal([]byte(doc), &role); err != nil {
			return nil, fmt.Errorf("%s does not parse as YAML: %w\n%s", what, err, doc)
		}
		if len(role.Rules) == 0 {
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
