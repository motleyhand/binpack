// Package rbacdoc reads binpack's RBAC out of the two documents that state
// it: the chart that renders the roles — through Helm, see [Render] — and the
// reference page an operator managing RBAC themselves writes a role from.
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

// CoreGroup is how a pair names Kubernetes' core API group, whose real name is
// the empty string.
//
// Not "core": that is a legal thing to write in a chart and an illegal thing
// for Kubernetes to resolve, so a pair has to be able to tell the two apart.
const CoreGroup = "(core)"

// RBACAPIVersion is the group and version Kubernetes serves RBAC objects
// under, and the only one a chart's roles may declare.
const RBACAPIVersion = RBACAPIGroup + "/v1"

// RBACAPIGroup is the only API group a roleRef may name. Kubernetes rejects a
// binding that names another, and the rejection is at install: roleRef is
// immutable, so there is no upgrade path that repairs it either.
const RBACAPIGroup = "rbac.authorization.k8s.io"

// SubjectAPIGroup is the API group a subject of the given kind must name.
//
// ServiceAccounts are core objects and take the empty group; Users and Groups
// are not objects at all and take the RBAC one. Kubernetes rejects a binding
// that gets this wrong, and the wrong value is invisible in every other field.
func SubjectAPIGroup(kind string) string {
	if kind == "ServiceAccount" {
		return ""
	}
	return RBACAPIGroup
}

// BindingKindFor is the binding kind that makes a role of the given kind
// effective as the chart intends.
//
// A ClusterRoleBinding may only reference a ClusterRole. A RoleBinding may
// reference either — and that is the trap: a RoleBinding naming a ClusterRole
// is legal, installs cleanly, and grants that ClusterRole's rules inside one
// namespace only. binpack's ClusterRole is the cluster-wide read of every Node
// and Pod, so bound that way it grants almost nothing binpack needs while
// every assertion about the rules themselves stays true.
func BindingKindFor(roleKind string) string {
	if roleKind == "ClusterRole" {
		return "ClusterRoleBinding"
	}
	return "RoleBinding"
}

// The two documents, spelled once. The prefix is relative to a package
// directory one level under the repository root, which is where every caller
// lives.
//
// The chart is a directory rather than the one template that holds the RBAC,
// because Helm renders a chart: the promises made about rbac.create and
// serviceAccount.create are about what an install produces, and a Role added
// to a second template would break them just as completely.
const (
	ChartDir      = "../../charts/binpack"
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
// [Grants] cannot see the difference. The name is what [Identity] matches on,
// since the distinguishing part of it is a literal suffix on a templated
// prefix. The namespace is comparable too — [HelmToYAML] derives each
// placeholder from the expression it replaces, so two different expressions
// stay two different scalars — but it is an expression, not a namespace, so it
// answers "are these two objects in the same place" and never "where".
type Role struct {
	// APIVersion is modelled for roleRef.apiGroup's reason, one level up: a
	// manifest missing it or misspelling it decodes to the same Kind, the same
	// name and the same rules, and Kubernetes refuses the object — so the
	// install has none of its permissions while every comparison here agrees.
	// Documented fragments carry none by design and are not held to it.
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Rules      []Rule   `json:"rules"`

	// Aggregation is present so that its presence can be refused. A
	// ClusterRole carrying an aggregationRule does not grant the rules written
	// in it: Kubernetes' aggregation controller owns that field and replaces
	// it with the union of every ClusterRole matching the selectors, so what
	// is written there is overwritten and what is granted is decided by labels
	// on objects this package never reads. Every comparison here would go on
	// inspecting rules the cluster ignores.
	Aggregation *struct{} `json:"aggregationRule"`
}

// Metadata is the identifying half of a role, which for these documents means
// the name alone: the namespace is a Helm expression by the time this reads it.
type Metadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// Binding is a RoleBinding or ClusterRoleBinding, as much of one as is needed
// to say which role it makes effective.
//
// A Role grants nothing until something binds it, and a binding naming a role
// that does not exist installs cleanly: the API server does not resolve
// roleRef at admission. So the autoscaler-status RoleBinding pointed at the
// leader-election Role's name is an install that comes up, holds its lease,
// and 403s on every ConfigMap read — with every assertion about what the Roles
// grant still true, because they do grant it and nothing reaches it.
type Binding struct {
	// APIVersion for the reason [Role] carries one: a binding missing it or
	// misspelling it decodes to the same kind, name, roleRef and subject, and
	// Kubernetes refuses the object — leaving the Role it names bound to
	// nobody while every assertion here agrees.
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	RoleRef    struct {
		// APIGroup is modelled because Kubernetes rejects a binding whose
		// roleRef names the wrong one, and nothing else here would notice: the
		// kind and name would still be the pair this package expects. Helm
		// validates neither, and roleRef is immutable, so the failure is at
		// install rather than at upgrade.
		APIGroup string `json:"apiGroup"`
		Kind     string `json:"kind"`
		Name     string `json:"name"`
	} `json:"roleRef"`

	// Subjects is who the binding grants to, which is the other half of
	// whether a permission is held. A binding naming the right role and the
	// wrong principal is as inert as one naming the wrong role, and reads the
	// same in every assertion about what the role grants.
	Subjects []struct {
		// APIGroup is modelled for roleRef's reason and a stricter one: it is
		// kind-specific. Kubernetes requires it to be empty for a
		// ServiceAccount, whose group is the core one, and
		// rbac.authorization.k8s.io for a User or a Group — so a subject that
		// acquires the wrong one decodes unchanged in every other field and is
		// rejected by the API server.
		APIGroup  string `json:"apiGroup"`
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"subjects"`
}

// Bindings decodes every binding in a set of rendered manifests.
func Bindings(manifests string) ([]Binding, error) {
	var out []Binding
	for _, doc := range documentSeparator.Split(manifests, -1) {
		var b Binding
		if err := yaml.Unmarshal([]byte(doc), &b); err != nil {
			return nil, fmt.Errorf("the chart's RBAC does not parse as YAML: %w\n%s", err, doc)
		}
		if b.Kind != "RoleBinding" && b.Kind != "ClusterRoleBinding" {
			continue
		}
		if b.APIVersion != RBACAPIVersion {
			return nil, fmt.Errorf("the chart declares a %s with apiVersion %q, and "+
				"Kubernetes serves RBAC under %q; the API server refuses the binding and "+
				"the Role it names is bound to nobody:\n%s",
				b.Kind, b.APIVersion, RBACAPIVersion, doc)
		}
		if b.Metadata.Name == "" {
			return nil, fmt.Errorf("the chart declares a %s with no metadata.name; the API "+
				"server refuses it at create, and nothing downstream reads a binding's own "+
				"name — it is checked through its roleRef and its subject, both of which "+
				"stay correct:\n%s", b.Kind, doc)
		}
		out = append(out, b)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("the chart declares no bindings at all")
	}
	return out, nil
}

// Kinds is the `kind` of every document in a set of rendered manifests.
//
// For the caller asking whether anything at all survives a guard. Matching
// `kind: Role` as text answers that for one serialisation of it: `kind:
// "Role"` is the same object to Kubernetes and to every reader here, and to a
// substring it is not there at all.
func Kinds(manifests string) ([]string, error) {
	objects, err := Objects(manifests)
	if err != nil {
		return nil, err
	}

	var out []string
	for _, object := range objects {
		out = append(out, object.Kind)
	}
	return out, nil
}

// Object is what a document says it is.
type Object struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
}

// Objects is every document in a set of rendered manifests, by what it
// declares itself to be.
//
// Decoded rather than matched, for the reason every other reader here decodes:
// `# kind: ServiceAccount` above a changed field satisfies a search for the
// text and describes an object the chart does not create.
func Objects(manifests string) ([]Object, error) {
	var out []Object
	for _, doc := range documentSeparator.Split(manifests, -1) {
		var object Object
		if err := yaml.Unmarshal([]byte(doc), &object); err != nil {
			return nil, fmt.Errorf("a rendered document does not parse as YAML: %w\n%s", err, doc)
		}
		if object.Kind != "" {
			out = append(out, object)
		}
	}
	return out, nil
}

// Roles decodes every role in a set of rendered manifests.
//
// One render is one set of values, so this is what an operator installing with
// those values actually gets. The question "which rules does this value turn
// on" is two renders and a difference, not a property of a single one.
func Roles(manifests string) ([]Role, error) {
	return decode("the chart's RBAC", manifests)
}

// fenceOpening matches the start of a fenced YAML block.
//
// `yml`, any capitalisation of either, a tilde fence, and whitespace before
// the info string — a renderer treats them all as YAML and a page is free to
// use any. Keyed on one exact spelling, a granted snippet written another way
// was skipped whole while the remaining blocks kept the non-empty guard
// satisfied.
//
// Up to three leading spaces, which is what CommonMark permits before a fence;
// a fourth makes it an indented code block instead.
var fenceOpening = regexp.MustCompile("(?i)^ {0,3}(`{3,}|~{3,})[ \t]*ya?ml[ \t]*$")

// fencedYAML is the body of every fenced YAML block in a Markdown document.
//
// Scanned line by line rather than matched with one expression, because the
// closing fence has to be the *same* delimiter as the opening one and RE2 has
// no backreference to say so. The expression this replaces accepted either
// delimiter as the close of either opener, anywhere on a line — so a backtick
// block whose body mentioned `~~~`, in a YAML comment or a piece of quoted
// output, ended there. What followed was not reported: the prefix already held
// a valid rule, so the block decoded, and every grant after that line was
// dropped in silence. That is the exact disappearance this package exists to
// prevent, in the reader written to prevent it.
//
// CommonMark's rules, as far as they matter here: the closing fence is the
// same character, at least as long as the opening run, alone on its line but
// for up to three leading spaces and trailing whitespace. An unclosed fence
// runs to the end of the document, which is a page nobody would ship and is
// read rather than refused — the decoders downstream say what is wrong with it
// far more usefully than "no closing fence" would.
func fencedYAML(doc string) []string {
	var out []string

	lines := strings.Split(doc, "\n")
	for i := 0; i < len(lines); i++ {
		opening := fenceOpening.FindStringSubmatch(lines[i])
		if opening == nil {
			continue
		}

		delimiter := opening[1]
		body := i + 1
		i = body
		for ; i < len(lines) && !closesFence(lines[i], delimiter); i++ {
		}
		out = append(out, strings.Join(lines[body:min(i, len(lines))], "\n"))
	}
	return out
}

// closesFence reports whether a line ends a fence opened with this delimiter.
func closesFence(line, delimiter string) bool {
	trimmed := strings.TrimRight(strings.TrimLeft(line, " "), " \t")
	if len(line)-len(strings.TrimLeft(line, " ")) > 3 {
		return false
	}
	// The same character, and at least as many: ```` closes ```, and ``` does
	// not close ````. Nothing else may share the line.
	if len(trimmed) < len(delimiter) {
		return false
	}
	return strings.Trim(trimmed, delimiter[:1]) == ""
}

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
	for _, body := range fencedYAML(doc) {
		role, err := fragment(body)
		// A block naming any rule field is a rule list, whatever became of it.
		// Keying that on `apiGroups:` alone skipped a non-resource snippet in
		// silence — it has no apiGroups by definition, so a malformed one was
		// read as prose quoted for discussion while an operator copying it got
		// an invalid role. Only in a rule list is a failed or empty decode a
		// dropped rule rather than an illustration, and only there is it
		// reported.
		rules := kindOfBlock(body) != "" || ruleField.MatchString(body)
		switch {
		case err != nil && rules:
			return nil, fmt.Errorf("a documented block is written as policy rules and does "+
				"not decode as them, so this reader would drop it: %w\n%s", err, body)
		case len(role.Rules) == 0 && rules:
			return nil, fmt.Errorf("a documented block is written as policy rules and "+
				"decoded to none, so this reader would drop it:\n%s", body)
		case err != nil, len(role.Rules) == 0:
			continue
		}

		// The same shape check the chart's rules get. A malformed rule
		// contributes no grant, so both directions of the comparison stay
		// green — while an operator assembling these snippets into a Role has
		// the whole object refused, which is the failure the page exists to
		// prevent.
		if why := malformed(role); why != "" {
			return nil, fmt.Errorf("a documented block declares a rule Kubernetes refuses: "+
				"%s. A role written from this page would be rejected whole:\n%s", why, body)
		}

		// And the refusal [decode] makes of the chart's own manifests, for the
		// same reason and with more at stake. A ClusterRole carrying an
		// aggregationRule does not grant what is written in it: the
		// aggregation controller replaces those rules with the union of every
		// ClusterRole its selectors match. The flattened grants can agree with
		// the chart exactly while an operator who assembled this role from the
		// page holds whatever labels elsewhere in their cluster decide —
		// possibly nothing. Checked here as well because this page is written
		// for the operator who is *not* using the chart, so nothing else in
		// their install would catch it.
		if role.Aggregation != nil {
			return nil, fmt.Errorf("a documented block declares an aggregationRule, so "+
				"Kubernetes decides its rules from labels on other ClusterRoles and the "+
				"rules shown here are overwritten — an operator assembling this role may "+
				"receive none of these permissions:\n%s", body)
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

// ruleField matches any field a policy rule or the object holding one carries.
//
// Quoting a mapping key is legal YAML and changes nothing about what the
// document means, so `"verbs": get` has to read as a rule field — keyed on the
// bare spelling alone, a malformed block written that way was skipped in
// silence. `kind` and `rules` are here for the same reason from the other
// side: a fence declaring itself a ClusterRole is a rule list whatever its
// rules turned out to be.
var ruleField = regexp.MustCompile(
	`(?m)^\s*-?\s*["']?(apiGroups|resources|resourceNames|nonResourceURLs|verbs|kind|rules)["']?\s*:`)

// kindOfBlock is the object a fragment's leading comment names, or "".
func kindOfBlock(body string) string {
	kind, _ := kindOf(body)
	return kind
}

// kindOf reads the leading comment a documented fragment names its object in,
// returning the kind and the whole line to identify it by.
func kindOf(body string) (kind, name string) {
	first, _, _ := strings.Cut(body, "\n")
	first = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(first), "#"))
	// A whole token, not a prefix. `RoleBinding` starts with `Role` and
	// `ClusterRoleBinding` with `ClusterRole`, so prefix matching read a
	// binding's header as the role it binds — leaving every grant comparison
	// unchanged while the page told an operator to put `rules:` on a binding,
	// which Kubernetes refuses.
	for _, kind := range []string{"ClusterRole", "Role"} {
		rest, ok := strings.CutPrefix(first, kind)
		if ok && (rest == "" || strings.HasPrefix(rest, ",") || strings.HasPrefix(rest, " ")) {
			return kind, first
		}
	}
	return "", ""
}

// Grants is every `group/resource: verb` triple the roles hold.
//
// The core group is rendered as a sentinel rather than the empty string, so a
// failure message names something a reader can place — and so that `events`
// from the core group and `events` from events.k8s.io stay different strings,
// which is the distinction a documented gap once hid in.
//
// Parenthesised because an API group is a DNS subdomain and cannot contain
// them. Spelled bare, `apiGroups: ["core"]` — a plausible mistake, and one
// Kubernetes reads as a group that does not exist — produced the same key as
// the empty group and agreed with everything.
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
					group = CoreGroup
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

// namelessVerbs are the verbs whose requests carry no object name however the
// client phrases them, so a resourceNames restriction on them authorises
// nothing at all.
//
// Two, and list and watch are not among them — which is what this repository
// asserted in three places until a reviewer checked. The API server derives
// the name from an exact-match metadata.name field selector when a collection
// request carries one (requestinfo.go in k8s.io/apiserver@v0.36.4 sets
// requestInfo.Name from opts.FieldSelector.RequiresExactMatch, ungated), and
// RuleAllows then matches resourceNames against it like any other name. So a
// name-restricted list is a grant Kubernetes deliberately supports, and
// refusing it here blocked the tightest rule an operator can write.
//
// A create carries no name because the object does not have one yet: the
// request path ends at the collection, requestInfo.Parts holds one element,
// and nothing later fills the field in. A deletecollection is the verb the
// server picks precisely when a delete arrived without a name. Neither has a
// selector escape hatch, so for these two the restriction is empty.
var namelessVerbs = []string{"create", "deletecollection"}

// Unauthorizable names grants that cannot authorise the request they appear
// to, however they are compared with anything else.
//
// This is the one property here that is not a comparison. Adding
// `resourceNames` to the eviction rule leaves chart and page agreeing, every
// count above its floor and every scope check satisfied — and no eviction is
// authorised, because a create is named by nobody at the moment it is
// authorised. A guard that only ever compares two documents cannot see a rule
// that is wrong on its own terms.
//
// It reports only what is wrong however the client behaves. Whether a
// name-restricted list authorises anything depends on the field selector the
// caller sends, which is not in either document this package reads, so it is
// not this function's to judge — see [namelessVerbs].
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

// Mistyped names the rendered documents whose metadata.name or
// metadata.namespace is not a YAML string.
//
// This is the one question here that a typed decode cannot answer, and the
// reason is worth keeping: `sigs.k8s.io/yaml` — which every other reader in
// this package goes through — converts YAML to JSON and then unmarshals, and
// it coerces a JSON bool into a string field without complaint. A Role
// rendered with `namespace: true` comes back from [Objects] as the string
// "true", identical to a correctly quoted one, so every comparison built on
// the decode is blind to the difference.
//
// The API server is not. Its manifest path decodes strictly and answers
// `cannot unmarshal bool into Go struct field ObjectMeta.metadata.namespace of
// type string`, so `kubectl apply` refuses that object while installing the
// others — verified against the strict serializer, not assumed from the
// lenient one that agrees with this package.
//
// That matters because `true`, `false` and `123` are legal DNS-1123 labels and
// therefore legal namespace names. An operator whose autoscaler runs in a
// namespace named `123` installs a chart that renders it unquoted and gets
// most of an install: the shape where it works until it does not.
//
// Decoded into `any` rather than matched as text, so this says what YAML the
// document is rather than what its bytes look like: a quoted scalar is a
// string whichever quoting style produced it.
func Mistyped(manifests string) ([]string, error) {
	var out []string
	for _, doc := range documentSeparator.Split(manifests, -1) {
		var object map[string]any
		if err := yaml.Unmarshal([]byte(doc), &object); err != nil {
			return nil, fmt.Errorf("a rendered document does not parse as YAML: %w\n%s", err, doc)
		}

		metadata, ok := object["metadata"].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"name", "namespace"} {
			value, present := metadata[field]
			if !present {
				continue
			}
			if _, ok := value.(string); !ok {
				out = append(out, fmt.Sprintf("%v %v has metadata.%s %v, a %T rather than "+
					"a string", object["kind"], metadata["name"], field, value, value))
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// Difference is what `on` grants that `off` does not — the grants a value
// turns on, as group/resource: verb pairs.
//
// Two renders and a subtraction, rather than reading the guarded block out of
// the template. A block lifted out of its document has no kind to carry, so
// reading it directly is the one way to obtain these pairs that cannot tell a
// ClusterRole rule from a namespaced one — and it answers for the text the
// guard encloses rather than for what Helm does with it, which are the same
// only until somebody writes the guard in a way the scanner did not expect.
//
// The caller narrows to a kind first when the question is scoped, because a
// pair granted cluster-wide in one render and namespaced in the other is a
// real change that a flat difference reports as nothing.
func Difference(on, off []Role) map[string]bool {
	before := Grants(off)

	out := map[string]bool{}
	for pair := range Grants(on) {
		if !before[pair] {
			out[pair] = true
		}
	}
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
//
// A separator may carry a comment or trailing space — `--- # the act rules` is
// a document start and so is `---   `. Matching only the bare form left an
// appended manifest inside the previous chunk, where a single-document
// unmarshal reads the first object and ignores the rest: a ClusterRole added
// after such a line rendered through Helm and was invisible to every audit
// here.
var documentSeparator = regexp.MustCompile(`(?m)^---[ \t]*(#.*)?$`)

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
		if role.Kind == "Role" || role.Kind == "ClusterRole" {
			if role.APIVersion != RBACAPIVersion {
				return nil, fmt.Errorf("%s declares a %s with apiVersion %q, and Kubernetes "+
					"serves RBAC under %q; the API server refuses the object and the "+
					"install has none of its permissions:\n%s",
					what, role.Kind, role.APIVersion, RBACAPIVersion, doc)
			}
		}
		if role.Metadata.Name == "" && (role.Kind == "Role" || role.Kind == "ClusterRole") {
			return nil, fmt.Errorf("%s declares a %s with no metadata.name; the API server "+
				"refuses it at create, and every check here keys on the name — an object "+
				"without one is a distinct identity, an unbound role and a silent "+
				"install failure all at once:\n%s", what, role.Kind, doc)
		}
		if why := malformed(role); why != "" {
			return nil, fmt.Errorf("%s declares a rule Kubernetes refuses: %s. The API "+
				"server rejects the whole role, so the install has none of its "+
				"permissions — and a rule that grants nothing contributes no pair here, "+
				"which every comparison reads as agreement:\n%s", what, why, doc)
		}
		if role.Aggregation != nil {
			return nil, fmt.Errorf("%s declares an aggregationRule, so Kubernetes decides "+
				"its rules from labels on other ClusterRoles and the rules written in it "+
				"are overwritten — every comparison here would be against a document the "+
				"cluster ignores:\n%s", what, doc)
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

// malformed describes the first rule of a role that Kubernetes would refuse,
// or "".
//
// Validated because the failure is silent in both directions at once: such a
// rule decodes without error and flattens to no grant, so every comparison,
// count and surplus check reads it as nothing to say — while the API server
// refuses the entire Role or ClusterRole and the install comes up with none of
// its permissions.
//
// The two shapes upstream allows are a resource rule, which names apiGroups
// and resources, and a non-resource rule, which names URLs and neither of the
// other two. Both need verbs; a rule with no verb authorises nothing it names.
func malformed(role Role) string {
	for i, rule := range role.Rules {
		switch {
		case len(rule.Verbs) == 0:
			return fmt.Sprintf("rule %d names no verbs", i+1)
		case len(rule.NonResourceURLs) > 0:
			// A namespaced Role cannot carry one at all, and a non-resource
			// rule may name none of the three resource fields — resourceNames
			// included, which an earlier version of this check overlooked
			// (k8s.io/kubernetes v1.36.4, pkg/apis/rbac/validation,
			// ValidatePolicyRule).
			if role.Kind == "Role" {
				return fmt.Sprintf("rule %d applies to non-resource URLs in a namespaced "+
					"Role, which cannot reach them", i+1)
			}
			if len(rule.Resources) > 0 || len(rule.APIGroups) > 0 || len(rule.ResourceNames) > 0 {
				return fmt.Sprintf("rule %d mixes nonResourceURLs with resource fields", i+1)
			}
		case len(rule.Resources) == 0:
			return fmt.Sprintf("rule %d names no resources and no nonResourceURLs", i+1)
		case len(rule.APIGroups) == 0:
			return fmt.Sprintf("rule %d names resources and no apiGroups", i+1)
		}
	}
	return ""
}
